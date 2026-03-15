package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr   string
	ServiceToken string

	// github.com/soulteary/gorge-task-queue connection
	TaskQueueURL   string
	TaskQueueToken string

	// Worker behaviour
	LeaseLimit     int // tasks per lease cycle
	PollIntervalMs int
	MaxWorkers     int // concurrent task processors
	IdleTimeoutSec int // hibernate after this many seconds idle

	// Conduit gateway (for workers that need to call back to Phorge)
	ConduitURL   string
	ConduitToken string

	// Only process these task classes (comma-separated); empty = all supported
	TaskClassFilter []string
}

func LoadFromEnv() *Config {
	return &Config{
		ListenAddr:      envStr("LISTEN_ADDR", ":8170"),
		ServiceToken:    envStr("SERVICE_TOKEN", ""),
		TaskQueueURL:    envStr("TASK_QUEUE_URL", "http://task-queue:8090"),
		TaskQueueToken:  envStr("TASK_QUEUE_TOKEN", ""),
		LeaseLimit:      envInt("LEASE_LIMIT", 4),
		PollIntervalMs:  envInt("POLL_INTERVAL_MS", 1000),
		MaxWorkers:      envInt("MAX_WORKERS", 4),
		IdleTimeoutSec:  envInt("IDLE_TIMEOUT_SEC", 180),
		ConduitURL:      envStr("CONDUIT_URL", ""),
		ConduitToken:    envStr("CONDUIT_TOKEN", ""),
		TaskClassFilter: splitCSV(envStr("TASK_CLASS_FILTER", "")),
	}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
