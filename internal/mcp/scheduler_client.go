package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// SchedulerHTTPClient is a thin HTTP client that satisfies AgentRouter by
// calling the scheduler's gateway endpoints. It is the implementation used
// by `ahsir mcp` so the stdio MCP shim never spawns or directly contacts
// agents — every request goes through the long-running scheduler process.
type SchedulerHTTPClient struct {
	baseURL string
	httpc   *http.Client
}

// defaultShimTimeout is the http.Client.Timeout used when the scheduler
// hasn't been queried for its own configured value yet, or returned
// something unparseable. It must be >= the scheduler's default chat
// timeout (10m); we add a small buffer so the shim doesn't kill a request
// the scheduler is still trying to fulfill.
const defaultShimTimeout = 11 * time.Minute

// NewSchedulerHTTPClient builds a client targeting the scheduler at baseURL
// (e.g. "http://127.0.0.1:9800"). The http.Client timeout is set to a safe
// default first so even immediate calls work, then RefreshTimeout (called
// by the caller after construction) aligns it with the scheduler's
// configured gateway timeout — so the operator only has to tune timeouts
// in ahsir.yaml, not in two places.
func NewSchedulerHTTPClient(baseURL string) *SchedulerHTTPClient {
	return &SchedulerHTTPClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpc:   &http.Client{Timeout: defaultShimTimeout},
	}
}

// RefreshTimeout asks the scheduler for its configured gateway chat timeout
// and bumps the client's http.Client.Timeout to that value plus a 1-minute
// buffer (so the shim never kills a request the scheduler would still
// honour). Called once at MCP shim startup.
//
// Best-effort: any error leaves the existing default in place. The shim
// logs to stderr so operators see when the alignment failed.
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
	c.httpc.Timeout = chat + time.Minute
	return c.httpc.Timeout, nil
}

// ListAgents returns the cards registered with the scheduler. On any error it
// returns nil — keeping the AgentRouter signature simple at the cost of
// hiding diagnostic detail. The MCP layer surfaces the empty list as "no
// agents available".
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
