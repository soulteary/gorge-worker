package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ConduitClient calls Phorge Conduit API methods (directly or via go-conduit).
type ConduitClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewConduitClient(baseURL, token string) *ConduitClient {
	return &ConduitClient{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type ConduitResponse struct {
	Result    json.RawMessage `json:"result"`
	ErrorCode *string         `json:"error_code"`
	ErrorInfo *string         `json:"error_info"`
}

func (c *ConduitClient) Call(ctx context.Context, method string, params map[string]any) (*ConduitResponse, error) {
	if params == nil {
		params = make(map[string]any)
	}
	params["__conduit__"] = true

	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}

	url := fmt.Sprintf("%s/api/%s", c.baseURL, method)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Service-Token", c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var cr ConduitResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if cr.ErrorCode != nil {
		info := ""
		if cr.ErrorInfo != nil {
			info = *cr.ErrorInfo
		}
		return nil, fmt.Errorf("conduit error [%s]: %s", *cr.ErrorCode, info)
	}

	return &cr, nil
}
