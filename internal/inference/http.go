package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPClient talks to an OpenAI-compatible /chat/completions endpoint.
type HTTPClient struct {
	BaseURL string // e.g. http://host:port/v1
	Model   string
	Timeout time.Duration
}

// Complete posts one chat completion and returns the assistant text.
func (c *HTTPClient) Complete(ctx context.Context, req Request) (Response, error) {
	if c.BaseURL == "" || c.Model == "" {
		return Response{}, fmt.Errorf("inference: base URL and model must be configured")
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.Prompt},
		},
		"max_tokens":  req.MaxTokens,
		"temperature": req.Temperature,
		"stream":      false,
	})
	if err != nil {
		return Response{}, err
	}

	url := strings.TrimSuffix(c.BaseURL, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("inference request: %w", err)
	}
	defer resp.Body.Close()
	// Bound the response: a step must not be able to run indefinitely or
	// exhaust memory on a runaway server.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read inference response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("inference status %d: %s", resp.StatusCode, truncate(string(raw), 512))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("decode inference response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("inference response has no choices")
	}
	return Response{
		Text:         out.Choices[0].Message.Content,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
		Truncated:    out.Choices[0].FinishReason == "length",
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
