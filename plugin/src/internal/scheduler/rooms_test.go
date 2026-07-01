package scheduler

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTurns is an injectable turnFunc: it records every call and answers from a
// per-agent reply function (so a turn's reply can be scripted or fixed).
type fakeTurns struct {
	mu    sync.Mutex
	calls []capturedTurn
	reply func(agent, message string) (string, error)
}

type capturedTurn struct {
	agent, contextID, speaker, message string
}

func (f *fakeTurns) run(agent, contextID, speaker, message string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, capturedTurn{agent, contextID, speaker, message})
	f.mu.Unlock()
	return f.reply(agent, message)
}

func (f *fakeTurns) messagesTo(agent string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.agent == agent {
			out = append(out, c.message)
		}
	}
	return out
}

// allAgents accepts any name as a registered agent.
func allAgents(string) bool { return true }

// waitForStatus polls the room until it reaches want (or fails on timeout).
func waitForStatus(t *testing.T, m *RoomManager, id string, want RoomStatus) *RoomView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := m.Get(id)
		if err == nil && v.Status == want {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := m.Get(id)
	t.Fatalf("room never reached %q (last: %+v)", want, v)
	return nil
}

func speakers(v *RoomView) []string {
	out := make([]string, len(v.Transcript))
	for i, tr := range v.Transcript {
		out[i] = tr.Speaker
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Test 1: an @-mention chain runs op->a->b, then parks waiting when b addresses
// no one (operator is the organizer).
func TestRoundtable_MentionChain(t *testing.T) {
	fake := &fakeTurns{reply: func(agent, _ string) (string, error) {
		switch agent {
		case "a":
			return "thinking... @b your take?", nil
		case "b":
			return "here's my answer, done.", nil
		}
		return "", fmt.Errorf("unexpected agent %q", agent)
	}}
	m := NewRoomManager(fake.run, allAgents)
	if _, err := m.CreateRoom("topic", []string{"a", "b"}, "operator", 8, "kick off @a"); err != nil {
		t.Fatal(err)
	}
	got := waitForStatus(t, m, onlyRoomID(t, m), RoomWaiting)
	if want := []string{"operator", "a", "b"}; !eq(speakers(got), want) {
		t.Fatalf("speakers = %v, want %v", speakers(got), want)
	}
	if got.Next != "" {
		t.Errorf("next = %q, want empty (parked)", got.Next)
	}
}

// Test 2: incremental relay — each agent's turn carries only unseen entries.
func TestRoundtable_IncrementalRelay(t *testing.T) {
	fake := &fakeTurns{reply: func(agent, _ string) (string, error) {
		switch agent {
		case "a":
			// a speaks twice: first hands to b, then (2nd turn) addresses no one.
			return "@b", nil
		case "b":
			return "@a back to you", nil
		}
		return "", nil
	}}
	// a@b -> b@a -> a(no mention) parks. a speaks on turns 1 and 3.
	fake.reply = scriptByAgent(map[string][]string{
		"a": {"@b", "no more"},
		"b": {"@a back"},
	})
	m := NewRoomManager(fake.run, allAgents)
	m.CreateRoom("T", []string{"a", "b"}, "operator", 8, "start @a")
	waitForStatus(t, m, onlyRoomID(t, m), RoomWaiting)

	aMsgs := fake.messagesTo("a")
	if len(aMsgs) != 2 {
		t.Fatalf("a got %d turns, want 2: %v", len(aMsgs), aMsgs)
	}
	// a's FIRST delta includes the operator opening; its SECOND delta covers
	// only b's new turn and must NOT repeat the opening.
	if !strings.Contains(aMsgs[0], "start") {
		t.Errorf("a's first delta should include the opening: %q", aMsgs[0])
	}
	if strings.Contains(aMsgs[1], "start") {
		t.Errorf("a's second delta must not repeat already-seen opening: %q", aMsgs[1])
	}
	if !strings.Contains(aMsgs[1], "b:") {
		t.Errorf("a's second delta should relay b's new turn: %q", aMsgs[1])
	}
}

// Test 3: organizer is an agent — a no-mention turn hands control to the
// moderator agent, not back to the operator.
func TestRoundtable_AgentOrganizer(t *testing.T) {
	fake := &fakeTurns{reply: scriptByAgent(map[string][]string{
		"a":   {"done"},
		"mod": {"@b please continue", "that wraps it up"},
		"b":   {"ok"},
	})}
	m := NewRoomManager(fake.run, allAgents)
	m.CreateRoom("T", []string{"a", "b", "mod"}, "mod", 8, "@a")
	got := waitForStatus(t, m, onlyRoomID(t, m), RoomWaiting)
	// op@a -> a"done"(no mention) -> organizer mod -> mod"@b" -> b"ok"(no mention)
	// -> organizer mod -> mod"that wraps"(no mention, speaker==organizer) -> park.
	want := []string{"operator", "a", "mod", "b", "mod"}
	if !eq(speakers(got), want) {
		t.Fatalf("speakers = %v, want %v", speakers(got), want)
	}
}

// Test 4: self-organizer guard — when the organizer agent itself addresses no
// one, the room parks (no organizer self-loop).
func TestRoundtable_SelfOrganizerParks(t *testing.T) {
	fake := &fakeTurns{reply: scriptByAgent(map[string][]string{
		"mod": {"I have no one to call on."},
	})}
	m := NewRoomManager(fake.run, allAgents)
	m.CreateRoom("T", []string{"a", "mod"}, "mod", 8, "@mod")
	got := waitForStatus(t, m, onlyRoomID(t, m), RoomWaiting)
	if want := []string{"operator", "mod"}; !eq(speakers(got), want) {
		t.Fatalf("speakers = %v, want %v", speakers(got), want)
	}
}

// Test 5: runaway guard — two agents @-mention each other forever; the
// autonomous chain stops at MaxChain, and an operator message resets it.
func TestRoundtable_RunawayGuard(t *testing.T) {
	fake := &fakeTurns{reply: func(agent, _ string) (string, error) {
		if agent == "a" {
			return "@b", nil
		}
		return "@a", nil
	}}
	m := NewRoomManager(fake.run, allAgents)
	m.CreateRoom("T", []string{"a", "b"}, "operator", 4, "go @a")
	got := waitForStatus(t, m, onlyRoomID(t, m), RoomWaiting)
	// 1 operator + exactly MaxChain(4) agent turns.
	if agentTurns := len(got.Transcript) - 1; agentTurns != 4 {
		t.Fatalf("autonomous chain ran %d agent turns, want MaxChain=4", agentTurns)
	}

	// Operator intervention resets the budget and resumes another bounded chain.
	m.Say(got.ID, "keep going @a", "operator")
	got2 := waitForStatus(t, m, got.ID, RoomWaiting)
	if agentTurns := len(got2.Transcript) - 2; agentTurns != 8 {
		t.Fatalf("after reset, total agent turns = %d, want 8 (4+4)", agentTurns)
	}
}

// Test 5b: a mention inside an inline code span or fenced code block is a
// QUOTED token, not an addressing directive — it must not schedule that agent.
// (Regression: an agent explaining the mechanism with `@teacher` in backticks
// wrongly handed the turn to teacher.)
func TestRoundtable_CodeSpanMentionsIgnored(t *testing.T) {
	parts := []string{"teacher", "student"}
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"inline code", "遇到不会的就 `@teacher` 求助，被 `@student` 点名后回答", nil},
		{"fenced block", "```\n@teacher please\n```\n机制说明完毕", nil},
		{"real handoff survives", "我说完了，@teacher 你怎么看？", []string{"teacher"}},
		{"code then real", "用法是 `@teacher`，现在 @teacher 真的请你发言", []string{"teacher"}},
	}
	for _, c := range cases {
		got := parseMentions(c.text, parts, "student")
		if !eq(got, c.want) {
			t.Errorf("%s: parseMentions = %v, want %v", c.name, got, c.want)
		}
	}
}

// Test 5c: while an agent's turn is in flight, the room exposes Speaking=<that
// agent> so the UI can show a "thinking" indicator. `Next` is cleared the moment
// the turn starts, so the indicator must key on Speaking, not Next.
func TestRoundtable_SpeakingDuringTurn(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	run := func(agent, _, _, _ string) (string, error) {
		close(entered)
		<-release // hold the turn so we can observe mid-flight state
		return "done", nil
	}
	m := NewRoomManager(run, allAgents)
	v, _ := m.CreateRoom("T", []string{"a", "b"}, "operator", 8, "@a")
	<-entered // a's turn is now executing

	mid, _ := m.Get(v.ID)
	if mid.Speaking != "a" {
		t.Fatalf("Speaking = %q during a's turn, want a", mid.Speaking)
	}
	if mid.Next != "" {
		t.Errorf("Next = %q mid-turn, want empty (consumed at turn start)", mid.Next)
	}

	close(release)
	done := waitForStatus(t, m, v.ID, RoomWaiting)
	if done.Speaking != "" {
		t.Errorf("Speaking = %q after turn, want empty", done.Speaking)
	}
}

// Test 5d: an overlong topic is truncated (rune-aware), not rejected.
func TestRoundtable_TopicTruncated(t *testing.T) {
	m := NewRoomManager((&fakeTurns{reply: scriptByAgent(nil)}).run, allAgents)
	long := strings.Repeat("话", 100) // 100 CJK runes, over the 60-rune cap
	v, err := m.CreateRoom(long, []string{"a"}, "operator", 8, "")
	if err != nil {
		t.Fatal(err)
	}
	r := []rune(v.Topic)
	if len(r) != 61 || r[60] != '…' { // 60 runes + ellipsis
		t.Fatalf("topic len = %d runes (want 61 incl. ellipsis): %q", len(r), v.Topic)
	}
	// A short topic is untouched.
	v2, _ := m.CreateRoom("简短话题", []string{"a"}, "operator", 8, "")
	if v2.Topic != "简短话题" {
		t.Fatalf("short topic altered: %q", v2.Topic)
	}
}

// Test 6: validation + non-participant mention.
func TestRoundtable_ValidationAndStrangerMention(t *testing.T) {
	onlyA := func(n string) bool { return n == "a" }
	m := NewRoomManager((&fakeTurns{reply: scriptByAgent(nil)}).run, onlyA)
	if _, err := m.CreateRoom("T", []string{"a", "ghost"}, "operator", 8, ""); err == nil {
		t.Fatal("expected error for non-registered participant")
	}
	// @stranger is not a participant -> no mention -> organizer(operator) -> wait.
	v, err := m.CreateRoom("T", []string{"a"}, "operator", 8, "hello @stranger")
	if err != nil {
		t.Fatal(err)
	}
	got := waitForStatus(t, m, v.ID, RoomWaiting)
	if len(got.Transcript) != 1 || got.Next != "" {
		t.Fatalf("stranger mention should not schedule anyone: %+v", got)
	}
}

// Test 7: stop is terminal.
func TestRoundtable_Stop(t *testing.T) {
	m := NewRoomManager((&fakeTurns{reply: scriptByAgent(map[string][]string{"a": {"hi"}})}).run, allAgents)
	v, _ := m.CreateRoom("T", []string{"a"}, "operator", 8, "")
	if _, err := m.Stop(v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Say(v.ID, "@a", "operator"); err == nil {
		t.Fatal("expected say on a stopped room to error")
	}
}

// --- helpers ---

// scriptByAgent returns a reply func that pops queued replies per agent; an
// empty/exhausted queue yields "" (no mention).
func scriptByAgent(scripts map[string][]string) func(agent, message string) (string, error) {
	var mu sync.Mutex
	q := map[string][]string{}
	for k, v := range scripts {
		q[k] = append([]string(nil), v...)
	}
	return func(agent, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(q[agent]) == 0 {
			return "", nil
		}
		r := q[agent][0]
		q[agent] = q[agent][1:]
		return r, nil
	}
}

func onlyRoomID(t *testing.T, m *RoomManager) string {
	t.Helper()
	rooms := m.List()
	if len(rooms) != 1 {
		t.Fatalf("expected exactly one room, got %d", len(rooms))
	}
	return rooms[0].ID
}
