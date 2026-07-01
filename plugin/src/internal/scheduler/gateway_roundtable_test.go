package scheduler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// TestGatewayRooms_CreateRoundtable verifies the POST /rooms dispatch + room view
// wiring for roundtable mode end-to-end over HTTP: a fixed-reply moderator that
// answers "CONSENSUS" forces a one-round consensus, the two participants both
// "同意", and the room parks with a moderator summary turn.
func TestGatewayRooms_CreateRoundtable(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	for _, a := range []struct{ name, reply string }{
		{"alice", "同意"},
		{"bob", "同意"},
		{"mod", "CONSENSUS"},
	} {
		url := realAgent(t, a.name, a.reply, 0)
		sch.Registry().Register(&a2a.AgentCard{
			Name:               a.name,
			URL:                url,
			PreferredTransport: a2a.TransportProtocolJSONRPC,
		})
	}

	body, _ := json.Marshal(map[string]any{
		"mode":         "roundtable",
		"topic":        "OKR",
		"participants": []string{"alice", "bob"},
		"moderator":    "mod",
		"message":      "请评审这份 OKR",
	})
	resp, err := http.Post(gwURL+"/rooms", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: status=%d body=%s", resp.StatusCode, raw)
	}
	var room RoomView
	json.NewDecoder(resp.Body).Decode(&room)
	resp.Body.Close()
	if room.Mode != RoomModeRoundtable || room.Moderator != "mod" {
		t.Fatalf("room view mode/moderator wrong: %+v", room)
	}

	// Round 1: both participants 同意, the moderator judges CONSENSUS and writes
	// a summary turn → the room parks. Transcript: operator, p1, p2, mod.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(gwURL + "/rooms/" + room.ID)
		if err != nil {
			t.Fatal(err)
		}
		var v RoomView
		json.NewDecoder(r.Body).Decode(&v)
		r.Body.Close()
		if v.Status == RoomWaiting && len(v.Transcript) == 4 {
			if v.Transcript[3].Speaker != "mod" {
				t.Fatalf("final turn should be the moderator summary: %+v", v.Transcript)
			}
			mids := map[string]bool{v.Transcript[1].Speaker: true, v.Transcript[2].Speaker: true}
			if !mids["alice"] || !mids["bob"] {
				t.Fatalf("both participants should have spoken: %+v", v.Transcript)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("roundtable did not reach consensus + summary in time")
}
