package taskqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type Client struct {
	baseURL    string
	token      string
	leaseOwner string
	httpClient *http.Client
}

func NewClient(baseURL, token string) *Client {
	hostname, _ := os.Hostname()
	pid := os.Getpid()
	leaseOwner := fmt.Sprintf("%s:%d:%d:github.com/soulteary/gorge-worker", hostname, pid, time.Now().Unix())

	return &Client{
		baseURL:    baseURL,
		token:      token,
		leaseOwner: leaseOwner,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type Task struct {
	ID            int64  `json:"id"`
	TaskClass     string `json:"taskClass"`
	LeaseOwner    string `json:"leaseOwner,omitempty"`
	LeaseExpires  *int64 `json:"leaseExpires,omitempty"`
	FailureCount  int    `json:"failureCount"`
	DataID        int64  `json:"dataID"`
	FailureTime   *int64 `json:"failureTime,omitempty"`
	Priority      int    `json:"priority"`
	ObjectPHID    string `json:"objectPHID,omitempty"`
	ContainerPHID string `json:"containerPHID,omitempty"`
	DateCreated   int64  `json:"dateCreated"`
	DateModified  int64  `json:"dateModified"`
	Data          string `json:"data,omitempty"`
}

type apiResponse struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error *apiError       `json:"error,omitempty"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) Lease(ctx context.Context, limit int) ([]*Task, error) {
	body, _ := json.Marshal(map[string]int{"limit": limit})
	req, err := c.newRequest(ctx, "POST", "/api/queue/lease", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lease-Owner", c.leaseOwner)

	var tasks []*Task
	if err := c.doJSON(req, &tasks); err != nil {
		return nil, fmt.Errorf("lease: %w", err)
	}
	return tasks, nil
}

func (c *Client) Complete(ctx context.Context, taskID int64, durationUs int64) error {
	body, _ := json.Marshal(map[string]int64{"taskID": taskID, "duration": durationUs})
	req, err := c.newRequest(ctx, "POST", "/api/queue/complete", body)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) Fail(ctx context.Context, taskID int64, permanent bool, retryWait *int) error {
	payload := map[string]any{
		"taskID":    taskID,
		"permanent": permanent,
	}
	if retryWait != nil {
		payload["retryWait"] = *retryWait
	}
	body, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, "POST", "/api/queue/fail", body)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) Yield(ctx context.Context, taskID int64, durationSec int) error {
	body, _ := json.Marshal(map[string]any{"taskID": taskID, "duration": durationSec})
	req, err := c.newRequest(ctx, "POST", "/api/queue/yield", body)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	url := c.baseURL + path
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("X-Service-Token", c.token)
	}
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var envelope apiResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("unmarshal (status %d): %w", resp.StatusCode, err)
	}

	if envelope.Error != nil {
		return fmt.Errorf("api error [%s]: %s", envelope.Error.Code, envelope.Error.Message)
	}

	if out != nil && envelope.Data != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("unmarshal data: %w", err)
		}
	}
	return nil
}
