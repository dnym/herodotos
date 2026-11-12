package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Scripted is a deterministic fake client. Replies are consumed in order;
// each entry must match the call's role exactly, which lets tests assert
// both content and call order.
type Scripted struct {
	Replies []Reply
	calls   []Request
}

// Reply is one scripted interaction. When Err is set the call fails.
// When Truncated is set the response reports as cut at the output limit,
// which lets tests exercise truncation handling.
type Reply struct {
	Role      string
	Text      string
	Err       error
	Truncated bool
}

// Complete implements Client.
func (f *Scripted) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if len(f.calls) >= len(f.Replies) {
		return Response{}, fmt.Errorf("inference fake: out of scripted replies, unexpected call for role %q", req.Role)
	}
	r := f.Replies[len(f.calls)]
	f.calls = append(f.calls, req)
	if r.Role != req.Role {
		return Response{}, fmt.Errorf("inference fake: role mismatch for call %d: want %q got %q", len(f.calls), r.Role, req.Role)
	}
	if r.Err != nil {
		return Response{}, r.Err
	}
	return Response{Text: r.Text, InputTokens: len(req.System) + len(req.Prompt), OutputTokens: len(r.Text), Truncated: r.Truncated}, nil
}

// Calls returns the requests made so far.
func (f *Scripted) Calls() []Request { return f.calls }

// LoadReplies reads a JSON array of {"role":..., "text":...} from path.
func LoadReplies(path string) ([]Reply, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse scripted replies %s: %w", path, err)
	}
	replies := make([]Reply, len(raw))
	for i, r := range raw {
		replies[i] = Reply{Role: r.Role, Text: strings.TrimSpace(r.Text)}
	}
	return replies, nil
}

// NewScriptedFromFile builds a Scripted client from a fixture file.
func NewScriptedFromFile(path string) (*Scripted, error) {
	replies, err := LoadReplies(path)
	if err != nil {
		return nil, err
	}
	return &Scripted{Replies: replies}, nil
}
