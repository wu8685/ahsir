package wrapper

import (
	"sync"

	"github.com/a2aproject/a2a-go/a2a"
)

// TaskStore is an in-memory store for A2A tasks.
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[a2a.TaskID]*a2a.Task
}

// NewTaskStore creates a new in-memory task store.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[a2a.TaskID]*a2a.Task),
	}
}

// Save stores a task.
func (ts *TaskStore) Save(task *a2a.Task) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.tasks[task.ID] = task
}

// Get retrieves a task by ID.
func (ts *TaskStore) Get(id a2a.TaskID) (*a2a.Task, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	task, ok := ts.tasks[id]
	return task, ok
}

// List returns all tasks.
func (ts *TaskStore) List() []*a2a.Task {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	tasks := make([]*a2a.Task, 0, len(ts.tasks))
	for _, t := range ts.tasks {
		tasks = append(tasks, t)
	}
	return tasks
}
