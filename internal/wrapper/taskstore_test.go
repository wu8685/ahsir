package wrapper

import (
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestTaskStoreListByContextID(t *testing.T) {
	ts := NewTaskStore()

	t1 := &a2a.Task{ID: "t1", ContextID: "ctx-A"}
	t2 := &a2a.Task{ID: "t2", ContextID: "ctx-B"}
	t3 := &a2a.Task{ID: "t3", ContextID: "ctx-A"}
	t4 := &a2a.Task{ID: "t4", ContextID: "ctx-A"}

	ts.Save(t1)
	ts.Save(t2)
	ts.Save(t3)
	ts.Save(t4)

	gotA := ts.ListByContextID("ctx-A")
	if len(gotA) != 3 {
		t.Fatalf("expected 3 tasks for ctx-A, got %d", len(gotA))
	}
	// Verify chronological (insertion) order is preserved.
	if gotA[0].ID != "t1" || gotA[1].ID != "t3" || gotA[2].ID != "t4" {
		t.Errorf("ctx-A insertion order wrong: %s, %s, %s", gotA[0].ID, gotA[1].ID, gotA[2].ID)
	}

	gotB := ts.ListByContextID("ctx-B")
	if len(gotB) != 1 || gotB[0].ID != "t2" {
		t.Errorf("expected only t2 for ctx-B, got %v", gotB)
	}

	if got := ts.ListByContextID(""); got != nil {
		t.Errorf("expected nil for empty contextID, got %v", got)
	}
	if got := ts.ListByContextID("ctx-missing"); got != nil {
		t.Errorf("expected nil for missing contextID, got %v", got)
	}
}

func TestTaskStoreSavePreservesOrderOnUpdate(t *testing.T) {
	ts := NewTaskStore()
	ts.Save(&a2a.Task{ID: "a", ContextID: "c"})
	ts.Save(&a2a.Task{ID: "b", ContextID: "c"})
	// Update existing — should not reshuffle.
	ts.Save(&a2a.Task{ID: "a", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}})

	got := ts.ListByContextID("c")
	if len(got) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("update reshuffled order: %s, %s", got[0].ID, got[1].ID)
	}
	if got[0].Status.State != a2a.TaskStateCompleted {
		t.Error("update did not replace the value")
	}
}
