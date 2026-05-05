package wrapper

import (
	"sync"

	"github.com/a2aproject/a2a-go/a2a"
)

// TaskStore is an in-memory store for A2A tasks.
// Tasks are kept in insertion order so callers can replay a context's history
// chronologically via ListByContextID.
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[a2a.TaskID]*a2a.Task
	order []a2a.TaskID
}

// NewTaskStore creates a new in-memory task store.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[a2a.TaskID]*a2a.Task),
	}
}

// Save stores a task. New IDs are appended to the insertion-order list;
// re-saving an existing ID updates the value and preserves the original
// position.
func (ts *TaskStore) Save(task *a2a.Task) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if _, exists := ts.tasks[task.ID]; !exists {
		ts.order = append(ts.order, task.ID)
	}
	ts.tasks[task.ID] = task
}

// Get retrieves a task by ID.
func (ts *TaskStore) Get(id a2a.TaskID) (*a2a.Task, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	task, ok := ts.tasks[id]
	return task, ok
}

// List returns all tasks in insertion order.
func (ts *TaskStore) List() []*a2a.Task {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	tasks := make([]*a2a.Task, 0, len(ts.order))
	for _, id := range ts.order {
		if t, ok := ts.tasks[id]; ok {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// ListByContextID returns all tasks sharing the given contextID, in insertion
// order. Returns nil for empty contextID or no matches — callers should treat
// nil as "no prior history".
func (ts *TaskStore) ListByContextID(contextID string) []*a2a.Task {
	if contextID == "" {
		return nil
	}
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	var out []*a2a.Task
	for _, id := range ts.order {
		t, ok := ts.tasks[id]
		if !ok {
			continue
		}
		if t.ContextID == contextID {
			out = append(out, t)
		}
	}
	return out
}
