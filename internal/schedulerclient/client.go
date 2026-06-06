package schedulerclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// SchedulerHTTPClient is a thin HTTP client for the scheduler's gateway
// endpoints. It powers the user-facing CLI (`ahsir list/chat/status/ping`) so
// Claude Code skills can drive a running scheduler through shell commands.
type SchedulerHTTPClient struct {
	baseURL string
	httpc   *http.Client
}

// defaultClientTimeout is the http.Client.Timeout used when the scheduler
// hasn't been queried for its own configured value yet, or returned
// something unparseable. It must be >= the scheduler's default chat
// timeout (10m); we add a small buffer so the client doesn't kill a request
// the scheduler is still trying to fulfill.
const defaultClientTimeout = 11 * time.Minute

// NewSchedulerHTTPClient builds a client targeting the scheduler at baseURL
// (e.g. "http://127.0.0.1:9800"). The http.Client timeout is set to a safe
// default first so even immediate calls work, then RefreshTimeout (called
// by the caller after construction) aligns it with the scheduler's
// configured gateway timeout — so the operator only has to tune timeouts
// in ahsir.yaml, not in two places.
func NewSchedulerHTTPClient(baseURL string) *SchedulerHTTPClient {
	return &SchedulerHTTPClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpc:   &http.Client{Timeout: defaultClientTimeout},
	}
}

// RefreshTimeout asks the scheduler for its configured gateway chat timeout
// and aligns the client's http.Client.Timeout with it. Positive chat timeouts
// get a 1-minute buffer so the CLI never kills a request the scheduler would
// still honour. A chat timeout of 0 means "no scheduler deadline", so the
// client also disables its field-level HTTP timeout.
//
// Best-effort: any error leaves the existing default in place.
func (c *SchedulerHTTPClient) RefreshTimeout() (time.Duration, error) {
	resp, err := c.httpc.Get(c.baseURL + "/config/timeouts")
	if err != nil {
		return c.httpc.Timeout, fmt.Errorf("fetch /config/timeouts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.httpc.Timeout, fmt.Errorf("scheduler %s on /config/timeouts", resp.Status)
	}
	var cfg struct {
		Chat string `json:"chat"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return c.httpc.Timeout, fmt.Errorf("decode /config/timeouts: %w", err)
	}
	chat, err := time.ParseDuration(cfg.Chat)
	if err != nil {
		return c.httpc.Timeout, fmt.Errorf("parse chat timeout %q: %w", cfg.Chat, err)
	}
	if chat == 0 {
		c.httpc.Timeout = 0
		return c.httpc.Timeout, nil
	}
	c.httpc.Timeout = chat + time.Minute
	return c.httpc.Timeout, nil
}

// ListAgents returns the cards registered with the scheduler. On any error it
// returns nil so CLI callers can treat an unreachable scheduler the same as an
// empty registry for display purposes.
func (c *SchedulerHTTPClient) ListAgents() []*a2a.AgentCard {
	resp, err := c.httpc.Get(c.baseURL + "/agents")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	// The registry serves cards with an extra "status" field embedded; that
	// field is ignored when decoding into a2a.AgentCard.
	var cards []*a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
		return nil
	}
	return cards
}

// ChatWithAgent forwards the message to the scheduler, which then forwards
// it to the agent over A2A. The response text is whatever the agent's
// reply renders to as a string.
//
// contextID, when non-empty, is set on the gateway request so the agent's
// SessionPool can reuse an existing session for that conversation. Empty
// contextID means each call is isolated (the agent's executor will
// auto-generate a fresh contextID per task).
func (c *SchedulerHTTPClient) ChatWithAgent(agentName, contextID, message string) (string, error) {
	payload := map[string]string{"message": message}
	if contextID != "" {
		payload["contextId"] = contextID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	endpoint := c.baseURL + "/agents/" + url.PathEscape(agentName) + "/chat"
	resp, err := c.httpc.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("scheduler request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", parseError(resp)
	}

	var out struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return out.Response, nil
}

// StreamWithAgent is the streaming counterpart of ChatWithAgent. It resolves
// the named agent's URL from the scheduler's /agents endpoint, opens an SSE
// connection to that agent's `message/stream` JSON-RPC method, and invokes
// onDelta for each token-level chunk as it arrives. The aggregated final
// response is returned when the stream terminates with a `kind: task`
// frame.
//
// Why direct-to-agent (not proxied through the scheduler): the SSE path is
// streaming-first, and the scheduler gateway's chat handler is buffered
// (returns a single chatResponse). Wiring a streaming proxy through the
// gateway is a much larger change; for the CLI's use case (interactive
// "typewriter" output) talking to the agent directly is the simplest
// honest implementation. The scheduler is still in the loop for agent
// discovery — `ahsir chat --stream` continues to require a running
// scheduler.
//
// onDelta is invoked SYNCHRONOUSLY from the SSE-reading goroutine; it must
// not block for long or the agent will appear to stall (the underlying
// http response body backpressures through the kernel socket buffer).
// Empty deltas (the initial "working" announcement with no text) are
// silently skipped — onDelta only fires for text-carrying frames.
//
// Returns the final aggregated response text plus any error. On error the
// partial aggregated text is still returned so callers can decide whether to
// surface it to the user.
func (c *SchedulerHTTPClient) StreamWithAgent(agentName, contextID, message string, onDelta func(string)) (string, error) {
	agentURL, err := c.resolveAgentURL(agentName)
	if err != nil {
		return "", err
	}
	return streamAgentSSE(c.httpc, agentURL, contextID, message, onDelta)
}

// resolveAgentURL fetches the agent's card from the scheduler registry and
// returns its base URL. Used by StreamWithAgent; exposed at this granularity
// so tests can stub the discovery half without spinning up a real agent.
func (c *SchedulerHTTPClient) resolveAgentURL(agentName string) (string, error) {
	endpoint := c.baseURL + "/agents/" + url.PathEscape(agentName)
	resp, err := c.httpc.Get(endpoint)
	if err != nil {
		return "", fmt.Errorf("resolve agent %q: %w", agentName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", parseError(resp)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return "", fmt.Errorf("decode agent card: %w", err)
	}
	if card.URL == "" {
		return "", fmt.Errorf("agent %q has no URL in card", agentName)
	}
	return card.URL, nil
}

// streamAgentSSE opens a JSON-RPC `message/stream` connection to agentURL
// and consumes the SSE response, calling onDelta per text-carrying frame.
// Returns the aggregated full response on terminal `kind: task` frame.
//
// Connection lifetime is not bounded by httpc.Timeout — that would chop
// off long streams. Caller must arrange external cancellation if they need
// it (the CLI honors SIGINT via its own context).
func streamAgentSSE(httpc *http.Client, agentURL, contextID, message string, onDelta func(string)) (string, error) {
	msgID := fmt.Sprintf("cli-stream-%d", time.Now().UnixNano())
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "message/stream",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": msgID,
				"contextId": contextID,
				"role":      "user",
				"parts":     []map[string]any{{"kind": "text", "text": message}},
			},
		},
		"id": 1,
	}
	body, _ := json.Marshal(payload)

	// Build a request that won't be killed by the outer http.Client.Timeout.
	// SSE streams legitimately live for minutes — relying on Timeout would
	// truncate every reply. Use a context.Background()-derived request and
	// trust the surrounding caller to enforce its own cancellation if needed.
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Use a transport without the outer Timeout (otherwise the body read
	// will be aborted partway through a long stream). Reuse httpc's
	// Transport so connection pooling / proxy settings still apply.
	streamClient := &http.Client{Transport: httpc.Transport, Timeout: 0}
	resp, err := streamClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("open stream: %w", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("expected SSE content-type, got %q; body: %s", ct, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var finalText strings.Builder
	sawTerminal := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimPrefix(data, " ")
		var env struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			return finalText.String(), fmt.Errorf("parse SSE envelope: %w; raw=%s", err, data)
		}
		if env.Error != nil {
			return finalText.String(), fmt.Errorf("JSON-RPC error %d: %s", env.Error.Code, env.Error.Message)
		}
		var head struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(env.Result, &head)
		switch head.Kind {
		case "status-update":
			var su struct {
				Status struct {
					Message *struct {
						Parts []struct {
							Kind string `json:"kind"`
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"message"`
				} `json:"status"`
			}
			if err := json.Unmarshal(env.Result, &su); err != nil {
				continue
			}
			if su.Status.Message == nil {
				continue
			}
			for _, p := range su.Status.Message.Parts {
				if p.Kind == "text" && p.Text != "" {
					if onDelta != nil {
						onDelta(p.Text)
					}
				}
			}
		case "task":
			// Terminal frame. The Task's last history entry (role=agent)
			// carries the canonical full response — extract it as the
			// return value so callers that ignored every delta still get
			// a usable answer.
			var task struct {
				History []struct {
					Role  string `json:"role"`
					Parts []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"history"`
			}
			if err := json.Unmarshal(env.Result, &task); err == nil {
				for i := len(task.History) - 1; i >= 0; i-- {
					h := task.History[i]
					if h.Role != "agent" {
						continue
					}
					for _, p := range h.Parts {
						if p.Kind == "text" && p.Text != "" {
							finalText.WriteString(p.Text)
							break
						}
					}
					break
				}
			}
			sawTerminal = true
		}
	}
	if err := scanner.Err(); err != nil {
		return finalText.String(), fmt.Errorf("read SSE: %w", err)
	}
	if !sawTerminal {
		return finalText.String(), errors.New("SSE stream ended without a terminal task frame")
	}
	return finalText.String(), nil
}

// GetTaskStatus retrieves the task representation from the scheduler.
func (c *SchedulerHTTPClient) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	endpoint := c.baseURL + "/agents/" + url.PathEscape(agentName) + "/tasks/" + url.PathEscape(taskID)
	resp, err := c.httpc.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("scheduler request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseError(resp)
	}

	var task a2a.Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, fmt.Errorf("decode task: %w", err)
	}
	return &task, nil
}

// parseError best-effort extracts the scheduler's JSON error message; falls
// back to status-line text if the body isn't shaped like {"error": "..."}.
func parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error != "" {
		return fmt.Errorf("scheduler %s: %s", resp.Status, e.Error)
	}
	return fmt.Errorf("scheduler %s: %s", resp.Status, strings.TrimSpace(string(body)))
}
