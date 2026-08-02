package scheduler

// Archived (offline) agent discovery + read-only transcript access.
//
// When an agent is deleted (`ahsir agent delete` / a cma-service session GC) it
// leaves the desired set and the registry, but its managed workspace under
// .ahsir/agents/<name> — including .a2a/transcripts/ — is preserved on disk. The
// console only lists live, registered agents, so the past conversation becomes
// unviewable once the process is gone.
//
// These helpers expose those workspaces read-only: they never re-spawn the agent
// and never mutate the workspace. Retention is honoured without pruning — a
// context whose most recent turn is past the 30-day window (see
// CompactForRetention / TranscriptExpired) is simply omitted, matching what the
// agent's own startup compaction would have dropped.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wu8685/ahsir/internal/wrapper"
)

// ArchivedContext summarises one on-disk transcript of an archived agent.
type ArchivedContext struct {
	ContextID    string `json:"contextId"`
	Title        string `json:"title"`
	Turns        int    `json:"turns"`
	LastActivity string `json:"lastActivity"`
	LastStatus   string `json:"lastStatus"`
}

// ArchivedAgent is an offline agent discovered on disk: a managed workspace
// whose agent is no longer registered/desired, but which still holds replayable
// transcripts.
type ArchivedAgent struct {
	Name     string            `json:"name"`
	Contexts []ArchivedContext `json:"contexts"`
}

// safeManagedName guards the mapping from a URL/path segment to a direct child
// of the managed-agents dir. Names come from on-disk dir entries or request
// paths; rejecting separators and traversal keeps filepath.Join from escaping
// the agents root.
func safeManagedName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return filepath.Base(name) == name
}

// isArchivable reports whether name is an offline agent: neither in the desired
// set (so the supervisor isn't trying to keep it up) nor currently registered.
// This excludes agents that are merely mid-restart, avoiding flicker between the
// live and archived views.
func (s *Scheduler) isArchivable(name string) bool {
	s.mu.Lock()
	_, desired := s.desired[name]
	s.mu.Unlock()
	if desired {
		return false
	}
	_, registered := s.registry.Get(name)
	return !registered
}

// ArchivedAgents enumerates offline agents under the managed-agents dir that
// still hold in-retention transcripts, each with a summary of its contexts.
// Newest-context-first within an agent; agents sorted by name. Read-only.
func (s *Scheduler) ArchivedAgents() ([]ArchivedAgent, error) {
	dir := s.cfg.ManagedAgentsDir()
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed agents dir: %w", err)
	}

	now := time.Now()
	out := make([]ArchivedAgent, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !safeManagedName(name) || !s.isArchivable(name) {
			continue
		}
		store := wrapper.NewTranscriptStore(filepath.Join(dir, name))
		summaries, err := store.ListContexts(now)
		if err != nil {
			// One unreadable workspace must not sink the whole list.
			log.Printf("archived: list contexts for %q failed: %v", name, err)
			continue
		}
		if len(summaries) == 0 {
			continue
		}
		contexts := make([]ArchivedContext, 0, len(summaries))
		for _, c := range summaries {
			contexts = append(contexts, ArchivedContext{
				ContextID:    c.ContextID,
				Title:        c.Title,
				Turns:        c.Turns,
				LastActivity: c.LastActivity.Format(time.RFC3339),
				LastStatus:   c.LastStatus,
			})
		}
		out = append(out, ArchivedAgent{Name: name, Contexts: contexts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ArchivedAgentHistory reads a single context's transcript for an offline agent
// straight from disk. It respects retention (a context past the window is
// reported as not found, mirroring what startup compaction would have pruned)
// and never mutates the workspace.
func (s *Scheduler) ArchivedAgentHistory(name, contextID string) ([]wrapper.TranscriptTurn, error) {
	return s.managedAgentHistory(name, contextID)
}

// managedAgentHistory reads a retained transcript from a scheduler-managed
// workspace regardless of whether the agent is archived, desired, or currently
// idle-stopped. It is deliberately read-only and never wakes the runtime.
func (s *Scheduler) managedAgentHistory(name, contextID string) ([]wrapper.TranscriptTurn, error) {
	dir := s.cfg.ManagedAgentsDir()
	if dir == "" {
		return nil, fmt.Errorf("no managed workspace directory")
	}
	if !safeManagedName(name) {
		return nil, fmt.Errorf("invalid agent name %q", name)
	}
	workspace := filepath.Join(dir, name)
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("archived agent %q not found", name)
	}
	turns, err := wrapper.NewTranscriptStore(workspace).Read(contextID)
	if err != nil {
		return nil, fmt.Errorf("read archived transcript: %w", err)
	}
	if len(turns) == 0 {
		return nil, fmt.Errorf("no transcript for context %q of agent %q", contextID, name)
	}
	if wrapper.TranscriptExpired(turns[len(turns)-1].TS, time.Now()) {
		return nil, fmt.Errorf("transcript for context %q of agent %q is past retention", contextID, name)
	}
	return turns, nil
}
