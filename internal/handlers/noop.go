package handlers

import (
	"context"
	"encoding/json"
	"log"

	"github.com/soulteary/gorge-worker/internal/taskqueue"
	"github.com/soulteary/gorge-worker/internal/worker"
)

// NewNoopHandler creates a handler that logs and completes the task.
// Used for tasks that only need to be acknowledged, or as a placeholder.
func NewNoopHandler(taskClass string) worker.TaskHandler {
	return func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error {
		log.Printf("[noop] completed taskClass=%s id=%d", taskClass, task.ID)
		return nil
	}
}
