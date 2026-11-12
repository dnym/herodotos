// Package inference abstracts model inference. Agent roles build prompts
// and decode text; transport stays here.
package inference

import "context"

// Request is a single completion call. System carries role instructions;
// Prompt carries the step context.
type Request struct {
	Role        string // logographer:<region-id>, orchestrator, summarizer
	System      string
	Prompt      string
	MaxTokens   int
	Temperature float64
}

// Response is the raw text plus usage where the backend reports it.
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
	// Truncated is set when the backend stopped at the output limit,
	// which almost always means structurally incomplete JSON.
	Truncated bool
}

// Client performs one completion call. Implementations must honor ctx
// deadlines and never panic on malformed upstream responses.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
