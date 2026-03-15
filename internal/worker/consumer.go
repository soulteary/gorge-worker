package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soulteary/gorge-worker/internal/taskqueue"
)

type Consumer struct {
	client       *taskqueue.Client
	registry     *Registry
	leaseLimit   int
	pollInterval time.Duration
	maxWorkers   int
	idleTimeout  time.Duration
	filter       map[string]bool

	processed atomic.Int64
	failed    atomic.Int64
	active    atomic.Int32
}

type ConsumerStats struct {
	Processed int64    `json:"processed"`
	Failed    int64    `json:"failed"`
	Active    int32    `json:"active"`
	Supported []string `json:"supported"`
}

func NewConsumer(
	client *taskqueue.Client,
	registry *Registry,
	leaseLimit int,
	pollIntervalMs int,
	maxWorkers int,
	idleTimeoutSec int,
	taskClassFilter []string,
) *Consumer {
	filter := make(map[string]bool, len(taskClassFilter))
	for _, tc := range taskClassFilter {
		filter[tc] = true
	}

	return &Consumer{
		client:       client,
		registry:     registry,
		leaseLimit:   leaseLimit,
		pollInterval: time.Duration(pollIntervalMs) * time.Millisecond,
		maxWorkers:   maxWorkers,
		idleTimeout:  time.Duration(idleTimeoutSec) * time.Second,
		filter:       filter,
	}
}

func (c *Consumer) Stats() ConsumerStats {
	return ConsumerStats{
		Processed: c.processed.Load(),
		Failed:    c.failed.Load(),
		Active:    c.active.Load(),
		Supported: c.registry.SupportedClasses(),
	}
}

func (c *Consumer) Run(ctx context.Context) {
	log.Printf("[worker] consumer started: lease_limit=%d poll=%s workers=%d supported=%v",
		c.leaseLimit, c.pollInterval, c.maxWorkers, c.registry.SupportedClasses())

	sem := make(chan struct{}, c.maxWorkers)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	var idleSince *time.Time

	for {
		select {
		case <-ctx.Done():
			log.Println("[worker] consumer shutting down")
			return
		case <-ticker.C:
			tasks, err := c.client.Lease(ctx, c.leaseLimit)
			if err != nil {
				log.Printf("[worker] lease error: %v", err)
				continue
			}

			if len(tasks) == 0 {
				if idleSince == nil {
					now := time.Now()
					idleSince = &now
				} else if c.idleTimeout > 0 && time.Since(*idleSince) > c.idleTimeout {
					log.Printf("[worker] idle for %s, hibernating...", c.idleTimeout)
					c.hibernate(ctx)
					idleSince = nil
				}
				continue
			}

			idleSince = nil
			var wg sync.WaitGroup
			for _, task := range tasks {
				if !c.shouldProcess(task) {
					log.Printf("[worker] skipping unsupported taskClass=%s id=%d",
						task.TaskClass, task.ID)
					retryWait := 60
					_ = c.client.Fail(ctx, task.ID, false, &retryWait)
					continue
				}

				sem <- struct{}{}
				wg.Add(1)
				go func(t *taskqueue.Task) {
					defer func() {
						<-sem
						wg.Done()
					}()
					c.processTask(ctx, t)
				}(task)
			}
			wg.Wait()
		}
	}
}

func (c *Consumer) shouldProcess(task *taskqueue.Task) bool {
	if len(c.filter) > 0 && !c.filter[task.TaskClass] {
		return false
	}
	return c.registry.Has(task.TaskClass)
}

func (c *Consumer) processTask(ctx context.Context, task *taskqueue.Task) {
	c.active.Add(1)
	defer c.active.Add(-1)

	start := time.Now()

	handler, ok := c.registry.Get(task.TaskClass)
	if !ok {
		log.Printf("[worker] no handler for taskClass=%s id=%d", task.TaskClass, task.ID)
		_ = c.client.Fail(ctx, task.ID, true, nil)
		c.failed.Add(1)
		return
	}

	var data json.RawMessage
	if task.Data != "" {
		data = json.RawMessage(task.Data)
	}

	err := handler(ctx, task, data)
	durationUs := time.Since(start).Microseconds()

	if err == nil {
		log.Printf("[worker] completed: taskClass=%s id=%d duration=%s",
			task.TaskClass, task.ID, time.Since(start))
		_ = c.client.Complete(ctx, task.ID, durationUs)
		c.processed.Add(1)
		return
	}

	var permErr *PermanentError
	var yieldErr *YieldError

	if errors.As(err, &permErr) {
		log.Printf("[worker] permanent failure: taskClass=%s id=%d err=%s",
			task.TaskClass, task.ID, err)
		_ = c.client.Fail(ctx, task.ID, true, nil)
		c.failed.Add(1)
	} else if errors.As(err, &yieldErr) {
		dur := yieldErr.Duration
		if dur < 5 {
			dur = 5
		}
		log.Printf("[worker] yield: taskClass=%s id=%d duration=%ds reason=%s",
			task.TaskClass, task.ID, dur, err)
		_ = c.client.Yield(ctx, task.ID, dur)
	} else {
		log.Printf("[worker] temporary failure: taskClass=%s id=%d err=%s",
			task.TaskClass, task.ID, err)
		_ = c.client.Fail(ctx, task.ID, false, nil)
		c.failed.Add(1)
	}
}

func (c *Consumer) hibernate(ctx context.Context) {
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
