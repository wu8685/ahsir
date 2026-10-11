package cmagateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/translate"
)

func TestParseGradeReply(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		check   func(t *testing.T, g *cma.OutcomeGrade)
	}{
		{
			name: "plain json",
			raw:  `{"verdict":"satisfied","criteria":[{"criterion":"c1","passed":true}],"explanation":"ok"}`,
			check: func(t *testing.T, g *cma.OutcomeGrade) {
				if g.Verdict != cma.OutcomeVerdictSatisfied || len(g.Criteria) != 1 || g.Explanation != "ok" {
					t.Fatalf("grade = %+v", g)
				}
			},
		},
		{
			name: "fenced json",
			raw:  "```json\n{\"verdict\":\"needs_revision\",\"criteria\":[{\"criterion\":\"c1\",\"passed\":false,\"feedback\":\"fix\"}]}\n```",
			check: func(t *testing.T, g *cma.OutcomeGrade) {
				if g.Verdict != cma.OutcomeVerdictNeedsRevision || g.Criteria[0].Feedback != "fix" {
					t.Fatalf("grade = %+v", g)
				}
			},
		},
		{
			name: "prose wrapped",
			raw:  "Here is my evaluation:\n{\"verdict\":\"satisfied\",\"criteria\":[]}\nHope that helps.",
			check: func(t *testing.T, g *cma.OutcomeGrade) {
				if g.Verdict != cma.OutcomeVerdictSatisfied {
					t.Fatalf("grade = %+v", g)
				}
			},
		},
		{name: "garbage", raw: "no json here", wantErr: true},
		{name: "invalid verdict", raw: `{"verdict":"maybe","criteria":[]}`, wantErr: true},
		{name: "missing verdict", raw: `{"criteria":[]}`, wantErr: true},
		{
			name: "unknown fields dropped",
			raw:  `{"verdict":"satisfied","criteria":[],"reasoning":"hidden","explanation":"e"}`,
			check: func(t *testing.T, g *cma.OutcomeGrade) {
				out, _ := json.Marshal(g)
				if strings.Contains(string(out), "hidden") {
					t.Fatalf("unknown grader fields must be dropped: %s", out)
				}
			},
		},
		{
			name: "oversized fields truncated",
			raw:  `{"verdict":"satisfied","criteria":[{"criterion":"` + strings.Repeat("c", 4096) + `","passed":true,"feedback":"` + strings.Repeat("f", 8192) + `"}],"explanation":"` + strings.Repeat("e", 16384) + `"}`,
			check: func(t *testing.T, g *cma.OutcomeGrade) {
				if len(g.Explanation) >= 16384 || len(g.Criteria[0].Criterion) >= 4096 || len(g.Criteria[0].Feedback) >= 8192 {
					t.Fatalf("fields not truncated: %+v", g)
				}
			},
		},
		{
			name: "criteria count capped",
			raw:  `{"verdict":"satisfied","criteria":[` + strings.Repeat(`{"criterion":"c","passed":true},`, 199) + `{"criterion":"c","passed":true}]}`,
			check: func(t *testing.T, g *cma.OutcomeGrade) {
				if len(g.Criteria) > 100 {
					t.Fatalf("criteria = %d, want capped at 100", len(g.Criteria))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := parseGradeReply(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGradeReply(%q) succeeded, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGradeReply: %v", err)
			}
			tc.check(t, g)
		})
	}
}

// chatCapture records /agents/{name}/chat requests as agent\nbody lines.
type chatCapture struct {
	mu     sync.Mutex
	bodies []string
	reply  string
}

func (c *chatCapture) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/agents/") && strings.HasSuffix(r.URL.Path, "/chat"):
		agent := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agents/"), "/chat")
		c.mu.Lock()
		c.bodies = append(c.bodies, agent+"\n"+string(readBody(r)))
		reply := c.reply
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"response": reply})
	default:
		http.NotFound(w, r)
	}
}

func graderTestAgent() *cma.Agent {
	return &cma.Agent{
		Type: "agent", ID: "agent_oc", Version: 2, Name: "oc",
		Model: cma.ModelConfig{ID: "test-model"}, Metadata: map[string]string{},
	}
}

func TestAhsirGrader_FreshContextPerCall(t *testing.T) {
	cap := &chatCapture{reply: `{"verdict":"satisfied","criteria":[]}`}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	agent := graderTestAgent()
	g := newAhsirGrader(ahsir.New(srv.URL, ""), translate.RuntimeDefaults{Provider: "test"}, agent, time.Minute)

	req := GradeRequest{OutcomeID: "outc_1", Goal: "g", Rubric: "r", Response: "resp"}
	if _, err := g.Grade(context.Background(), req); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if _, err := g.Grade(context.Background(), req); err != nil {
		t.Fatalf("Grade: %v", err)
	}

	cap.mu.Lock()
	bodies := append([]string(nil), cap.bodies...)
	cap.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("chat calls = %d, want 2", len(bodies))
	}
	contexts := map[string]bool{}
	for _, b := range bodies {
		if !strings.HasPrefix(b, translate.AhsirGraderName(agent.ID, agent.Version)+"\n") {
			t.Fatalf("grader chat went to unexpected agent: %q", strings.SplitN(b, "\n", 2)[0])
		}
		var payload struct {
			ContextID string `json:"contextId"`
		}
		if err := json.Unmarshal([]byte(strings.SplitN(b, "\n", 2)[1]), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ContextID == "" {
			t.Fatal("grader chat sent no contextId")
		}
		contexts[payload.ContextID] = true
	}
	if len(contexts) != 2 {
		t.Fatalf("grader reused a contextId across calls: %v", contexts)
	}
}

func TestAhsirGrader_RegisterFailureIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no runtime"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	g := newAhsirGrader(ahsir.New(srv.URL, ""), translate.RuntimeDefaults{Provider: "test"}, graderTestAgent(), time.Minute)
	if _, err := g.Grade(context.Background(), GradeRequest{OutcomeID: "o", Goal: "g", Rubric: "r"}); err == nil {
		t.Fatal("Grade succeeded despite grader provisioning failure")
	}
}

func TestAhsirGrader_UnparseableReplyIsError(t *testing.T) {
	cap := &chatCapture{reply: "I cannot decide."}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	g := newAhsirGrader(ahsir.New(srv.URL, ""), translate.RuntimeDefaults{Provider: "test"}, graderTestAgent(), time.Minute)
	if _, err := g.Grade(context.Background(), GradeRequest{OutcomeID: "o", Goal: "g", Rubric: "r"}); err == nil {
		t.Fatal("Grade succeeded on an unparseable grader reply")
	}
}

func TestTruncateForOutcome(t *testing.T) {
	if got := truncateForOutcome("short", 10); got != "short" {
		t.Fatalf("truncateForOutcome short = %q", got)
	}
	got := truncateForOutcome(strings.Repeat("x", 100), 10)
	if len(got) > 64 || !strings.Contains(got, "truncated") {
		t.Fatalf("truncateForOutcome long = %q (len %d)", got, len(got))
	}
	if got := truncateForOutcome("", 10); got != "" {
		t.Fatalf("truncateForOutcome empty = %q", got)
	}
}

func TestTruncateForOutcomeUTF8Boundary(t *testing.T) {
	// "界" is 3 bytes; max=8 lands mid-rune (2 full runes = 6 bytes).
	got := truncateForOutcome(strings.Repeat("界", 10), 8)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForOutcome produced invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "界界") {
		t.Fatalf("truncateForOutcome = %q, want 2 full runes kept", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncateForOutcome = %q, want truncation marker", got)
	}
}

func TestAhsirGrader_InvalidRuntimeMetadataIsError(t *testing.T) {
	// An invalid runtime_api_key_env fails card translation at construction;
	// Grade must surface it rather than registering a broken grader.
	agent := graderTestAgent()
	agent.Metadata = map[string]string{"runtime_api_key_env": "not a shell name!"}
	g := newAhsirGrader(ahsir.New("http://127.0.0.1:0", ""), translate.RuntimeDefaults{}, agent, time.Minute)
	if _, err := g.Grade(context.Background(), GradeRequest{OutcomeID: "o", Goal: "g", Rubric: "r"}); err == nil {
		t.Fatal("Grade succeeded with an invalid grader card")
	}
}
