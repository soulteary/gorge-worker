package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/soulteary/gorge-worker/internal/taskqueue"
	"github.com/soulteary/gorge-worker/internal/worker"
)

// NewConduitDelegateHandler creates a handler that delegates task execution
// to the Phorge PHP backend via the internal worker.execute Conduit method.
//
// This allows github.com/soulteary/gorge-worker to act as the task consumer (replacing PhabricatorTaskmasterDaemon)
// while letting PHP handle the actual business logic for complex task types.
func NewConduitDelegateHandler(conduit *ConduitClient) worker.TaskHandler {
	return func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error {
		params := map[string]any{
			"taskID":    task.ID,
			"taskClass": task.TaskClass,
			"data":      string(data),
		}

		result, err := conduit.Call(ctx, "worker.execute", params)
		if err != nil {
			return fmt.Errorf("conduit worker.execute for %s (id=%d): %w",
				task.TaskClass, task.ID, err)
		}

		log.Printf("[delegate] taskClass=%s id=%d delegated via conduit, result=%s",
			task.TaskClass, task.ID, string(result.Result))
		return nil
	}
}
