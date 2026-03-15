package worker

import (
	"context"
	"encoding/json"

	"github.com/soulteary/gorge-worker/internal/taskqueue"
)

// TaskHandler processes a single task. Implementations should return:
//   - nil for success
//   - *PermanentError for permanent failure (task won't retry)
//   - *YieldError to yield and retry later
//   - any other error for temporary failure (task retries)
type TaskHandler func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error

type PermanentError struct {
	Msg string
}

func (e *PermanentError) Error() string { return e.Msg }

type YieldError struct {
	Msg      string
	Duration int // seconds, minimum 5
}

func (e *YieldError) Error() string { return e.Msg }

type Registry struct {
	handlers map[string]TaskHandler
}

func NewRegistry() *Registry {
	return &Registry{
		handlers: make(map[string]TaskHandler),
	}
}

func (r *Registry) Register(taskClass string, handler TaskHandler) {
	r.handlers[taskClass] = handler
}

func (r *Registry) Get(taskClass string) (TaskHandler, bool) {
	h, ok := r.handlers[taskClass]
	return h, ok
}

func (r *Registry) Has(taskClass string) bool {
	_, ok := r.handlers[taskClass]
	return ok
}

func (r *Registry) SupportedClasses() []string {
	classes := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		classes = append(classes, k)
	}
	return classes
}
