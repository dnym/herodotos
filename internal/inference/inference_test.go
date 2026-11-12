package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPClientComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["model"] != "test-model" {
			t.Errorf("model = %v", body["model"])
		}
		resp := map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": `{"none":true}`},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 4},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL + "/v1", Model: "test-model", Timeout: 5 * time.Second}
	got, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p", MaxTokens: 100, Temperature: 0.7})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Text != `{"none":true}` {
		t.Errorf("text = %q", got.Text)
	}
	if got.InputTokens != 10 || got.OutputTokens != 4 {
		t.Errorf("usage = %d/%d", got.InputTokens, got.OutputTokens)
	}
}

func TestHTTPClientErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL + "/v1", Model: "m"}
	if _, err := c.Complete(context.Background(), Request{}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestScriptedRoleMismatch(t *testing.T) {
	f := &Scripted{Replies: []Reply{{Role: "logographer:a", Text: `{"none":true}`}}}
	if _, err := f.Complete(context.Background(), Request{Role: "logographer:b"}); err == nil {
		t.Fatal("expected role mismatch error")
	}
}
