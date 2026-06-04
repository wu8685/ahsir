// Package example_test demonstrates loading agent card configs and running a multi-agent setup.
//
// Run: go test ./example/ -v
package example_test

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/scheduler"
	"github.com/wu8685/ahsir/internal/wrapper"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestLoadAgentCards verifies that both agent card YAML files are valid.
func TestLoadAgentCards(t *testing.T) {
	teacherBuilder := wrapper.NewAgentCardBuilder("workspaces/teacher")
	teacherCfg, err := teacherBuilder.Load()
	if err != nil {
		t.Fatalf("load teacher card: %v", err)
	}
	if teacherCfg.Name != "teacher" {
		t.Errorf("expected teacher, got %s", teacherCfg.Name)
	}
	if len(teacherCfg.Skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(teacherCfg.Skills))
	}
	if !teacherCfg.Filesystem.Enabled {
		t.Error("expected teacher filesystem to be enabled")
	}
	t.Logf("Teacher card: %s (skills: teaching, summarization, fs: enabled)", teacherCfg.Name)

	studentBuilder := wrapper.NewAgentCardBuilder("workspaces/student")
	studentCfg, err := studentBuilder.Load()
	if err != nil {
		t.Fatalf("load student card: %v", err)
	}
	if studentCfg.Name != "student" {
		t.Errorf("expected student, got %s", studentCfg.Name)
	}
	if studentCfg.Claude.MaxAgentCalls != 3 {
		t.Errorf("expected maxAgentCalls=3, got %d", studentCfg.Claude.MaxAgentCalls)
	}
	if !studentCfg.Filesystem.Enabled {
		t.Error("expected student filesystem to be enabled")
	}
	t.Logf("Student card: %s (maxAgentCalls: %d, fs: enabled)", studentCfg.Name, studentCfg.Claude.MaxAgentCalls)
}

// TestLoadSchedulerConfig verifies the ahsir.yaml config.
func TestLoadSchedulerConfig(t *testing.T) {
	cfg, err := scheduler.LoadConfig("ahsir.yaml")
	if err != nil {
		t.Fatalf("load scheduler config: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(cfg.Agents))
	}
	if cfg.Registry.Port != 9800 {
		t.Errorf("expected registry port 9800, got %d", cfg.Registry.Port)
	}
	t.Logf("Scheduler config: %d agents, registry at %s:%d",
		len(cfg.Agents), cfg.Registry.Host, cfg.Registry.Port)

	names := map[string]bool{}
	for _, a := range cfg.Agents {
		names[a.Name] = true
	}
	if !names["teacher"] || !names["student"] {
		t.Error("expected both teacher and student in config")
	}
}

// TestStudentDelegatesToTeacher is the core integration test: student receives a message,
// delegates to teacher via ---A2A_CALL---, and returns the teacher's answer to the user.
func TestStudentDelegatesToTeacher(t *testing.T) {
	// ---- Setup Teacher agent ----
	teacherTasks := wrapper.NewTaskStore()
	teacherExec := func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
		task := a2a.NewSubmittedTask(a2a.TaskInfo{}, msg)
		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		task.History = []*a2a.Message{
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{
				Text: "This article discusses Go concurrency patterns including goroutines, channels, and the select statement.",
			}),
		}
		return task, nil
	}
	teacherA2A := wrapper.NewA2AServer(teacherTasks, teacherExec)
	teacherHTTP := httptest.NewServer(teacherA2A)
	defer teacherHTTP.Close()

	// ---- Setup Student agent with executor ----
	studentTasks := wrapper.NewTaskStore()
	studentSender := func(ctx context.Context, prompt string) (string, error) {
		if strings.Contains(prompt, "[Agent teacher returned:") {
			return "Based on the teacher's analysis, the article covers Go concurrency patterns.\n", nil
		}
		return strings.Join([]string{
			"Let me ask the teacher about this.",
			"---A2A_CALL---",
			`{"agent": "teacher", "task": "Please summarize the article about Go concurrency"}`,
			"---END---",
		}, "\n") + "\n", nil
	}
	studentExec := wrapper.NewExecutor(wrapper.ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (wrapper.Session, error) {
			return wrapper.NewOneshotSession(studentSender), nil
		},
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{{
				Name:              "teacher",
				URL:               teacherHTTP.URL,
				PreferredTransport: a2a.TransportProtocolJSONRPC,
				Skills:            []a2a.AgentSkill{{Name: "teaching"}, {Name: "summarization"}},
			}}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			client, err := wrapper.NewAgentClient(ctx, &a2a.AgentCard{
				Name:              "teacher",
				URL:               teacherHTTP.URL,
				PreferredTransport: a2a.TransportProtocolJSONRPC,
			})
			if err != nil {
				return "", err
			}
			return client.SendMessage(ctx, contextID, task)
		},
		MaxDepth:   3,
		BasePrompt: "You are a Student. Ask the teacher when you need help.",
	})
	studentA2A := wrapper.NewA2AServer(studentTasks, studentExec.Execute)
	studentHTTP := httptest.NewServer(studentA2A)
	defer studentHTTP.Close()

	// ---- Setup Scheduler ----
	cfg := &scheduler.Config{
		Registry: scheduler.RegistryConfig{
			Host:              "127.0.0.1",
			Port:              freePort(t),
			HeartbeatInterval: "10s",
			HeartbeatTimeout:  "30s",
		},
		PortRange: scheduler.PortRange{Start: 9801, End: 9900},
	}
	sch := scheduler.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	sch.Registry().Register(&a2a.AgentCard{
		Name: "student", URL: studentHTTP.URL, PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	sch.Registry().Register(&a2a.AgentCard{
		Name: "teacher", URL: teacherHTTP.URL, PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	// ---- Test 1: Student delegates to teacher ----
	t.Log("=== Test 1: Student delegates to teacher ===")
	resp, err := sch.ChatWithAgent("student", "Summarize the article")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Student: %s", resp)
	if !strings.Contains(resp, "teacher") {
		t.Error("expected student to mention consulting the teacher")
	}

	// ---- Test 2: Direct teacher chat ----
	t.Log("=== Test 2: Ask teacher directly ===")
	resp2, err := sch.ChatWithAgent("teacher", "What is a goroutine?")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Teacher: %s", resp2)
	if resp2 == "" {
		t.Error("expected non-empty teacher response")
	}
}
