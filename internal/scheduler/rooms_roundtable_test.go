package scheduler

import (
	"strings"
	"sync"
	"testing"
)

// modReply scripts the engine's injected run(): a participant ("a"/"b"/...)
// always agrees; the moderator ("mod") returns the i-th scripted consolidation
// for its i-th call (one per round), defaulting to "CONTINUE" past the list.
func modReply(consolidations ...string) func(agent, message string) (string, error) {
	var mu sync.Mutex
	n := 0
	return func(agent, message string) (string, error) {
		if agent != "mod" {
			return "同意", nil
		}
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		if i < len(consolidations) {
			return consolidations[i], nil
		}
		return "CONTINUE", nil
	}
}

// Consensus in one round: participants agree, the moderator's consolidation
// carries a CONSENSUS verdict, and the room parks with the consolidation as the
// final visible turn. Pins contextId isolation (mod uses roomID+"#mod").
func TestRoundtable_ConsensusOneRound(t *testing.T) {
	fake := &fakeTurns{reply: modReply("【已达成】OKR 通过\nCONSENSUS")}
	m := NewRoomManager(fake.run, allAgents)
	m.shuffle = func([]string) {} // deterministic order: a, b, c

	v, err := m.CreateRoundtableRoom("OKR 设计", []string{"a", "b", "c"}, "mod", 0, "请评审这份 OKR")
	if err != nil {
		t.Fatal(err)
	}
	got := waitForStatus(t, m, v.ID, RoomWaiting)

	want := []string{"operator", "a", "b", "c", "mod"}
	if !eq(speakers(got), want) {
		t.Fatalf("speakers = %v, want %v", speakers(got), want)
	}
	if last := got.Transcript[len(got.Transcript)-1]; last.Speaker != "mod" || last.Text != "【已达成】OKR 通过" {
		t.Fatalf("final turn = %+v, want the moderator consolidation (verdict line stripped)", last)
	}
	for _, c := range fake.calls {
		if c.agent == "mod" && c.contextID != got.ID+"#mod" {
			t.Errorf("moderator call used contextId %q, want %q (isolation)", c.contextID, got.ID+"#mod")
		}
		if c.agent != "mod" && c.agent != "operator" && c.contextID != got.ID {
			t.Errorf("participant %q used contextId %q, want room id %q", c.agent, c.contextID, got.ID)
		}
	}
}

// Two rounds: a CONTINUE consolidation after round 1 (appended, anchors round
// 2), a CONSENSUS consolidation after round 2. Each round = a full pass + the
// moderator's consolidation turn.
func TestRoundtable_TwoRoundsToConsensus(t *testing.T) {
	fake := &fakeTurns{reply: modReply("CONTINUE", "CONSENSUS")}
	m := NewRoomManager(fake.run, allAgents)
	m.shuffle = func([]string) {} // order a, b each round

	v, _ := m.CreateRoundtableRoom("T", []string{"a", "b"}, "mod", 0, "go")
	got := waitForStatus(t, m, v.ID, RoomWaiting)

	want := []string{"operator", "a", "b", "mod", "a", "b", "mod"}
	if !eq(speakers(got), want) {
		t.Fatalf("speakers = %v, want %v", speakers(got), want)
	}

	// Round 2's participants must see round 1's consolidation in their delta.
	aMsgs := fake.messagesTo("a")
	if len(aMsgs) < 2 || !strings.Contains(aMsgs[1], "CONTINUE") {
		t.Fatalf("round-2 'a' should see the round-1 consolidation; got %q", aMsgs)
	}
}

// No consensus within Budget rounds: each round still appends a consolidation,
// but the room parks for the operator (last consolidation is not CONSENSUS).
func TestRoundtable_BudgetParks(t *testing.T) {
	fake := &fakeTurns{reply: func(agent, message string) (string, error) {
		if agent == "mod" {
			return "【待议】仍有分歧\nCONTINUE", nil // never converges
		}
		return "我还有意见", nil
	}}
	m := NewRoomManager(fake.run, allAgents)
	m.shuffle = func([]string) {}

	v, _ := m.CreateRoundtableRoom("T", []string{"a", "b"}, "mod", 2, "go") // budget = 2 rounds
	got := waitForStatus(t, m, v.ID, RoomWaiting)

	want := []string{"operator", "a", "b", "mod", "a", "b", "mod"}
	if !eq(speakers(got), want) {
		t.Fatalf("speakers = %v, want %v", speakers(got), want)
	}
}

// A later speaker in a round sees earlier same-round turns (shared broadcast).
func TestRoundtable_SameRoundVisibility(t *testing.T) {
	fake := &fakeTurns{reply: func(agent, message string) (string, error) {
		switch agent {
		case "mod":
			return "CONSENSUS", nil
		case "a":
			return "A 的观点", nil
		default:
			return "同意", nil
		}
	}}
	m := NewRoomManager(fake.run, allAgents)
	m.shuffle = func([]string) {} // order a, b

	v, _ := m.CreateRoundtableRoom("T", []string{"a", "b"}, "mod", 0, "go")
	waitForStatus(t, m, v.ID, RoomWaiting)

	bMsgs := fake.messagesTo("b")
	if len(bMsgs) == 0 || !strings.Contains(bMsgs[0], "A 的观点") {
		t.Fatalf("b's first turn should see a's same-round turn; got %q", bMsgs)
	}
}

// A roundtable rejects a moderator that is also a participant, and requires one.
func TestRoundtable_ModeratorValidation(t *testing.T) {
	m := NewRoomManager((&fakeTurns{reply: modReply("CONSENSUS")}).run, allAgents)
	if _, err := m.CreateRoundtableRoom("T", []string{"a", "b"}, "", 0, ""); err == nil {
		t.Error("expected error: roundtable needs a moderator")
	}
	if _, err := m.CreateRoundtableRoom("T", []string{"a", "b"}, "a", 0, ""); err == nil {
		t.Error("expected error: moderator must not also be a participant")
	}
}
