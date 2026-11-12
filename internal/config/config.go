// Package config centralizes budgets and runtime configuration. Limits
// exist so a step cannot run indefinitely, exhaust tokens, or negotiate
// forever.
package config

import (
	"os"
	"time"
)

// Budgets bounds one simulation step. Defaults follow the MVP policy:
// two reaction/revision rounds and two correction retries.
type Budgets struct {
	MaxReactionRounds    int
	MaxCorrectionRetries int
	MaxModelCallsPerStep int
	MaxOutputTokens      int
	MaxPromptBytes       int
}

// DefaultBudgets returns the provisional MVP defaults.
func DefaultBudgets() Budgets {
	return Budgets{
		MaxReactionRounds:    2,
		MaxCorrectionRetries: 2,
		MaxModelCallsPerStep: 20,
		MaxOutputTokens:      1600,
		MaxPromptBytes:       120_000,
	}
}

// Inference describes the model endpoint. Credentials stay in the
// environment; nothing secret is stored in the database or the repo.
type Inference struct {
	BaseURL string
	Model   string
	Timeout time.Duration
}

// InferenceFromEnv reads HERODOTOS_INFERENCE_BASE_URL,
// HERODOTOS_INFERENCE_MODEL, and an optional HERODOTOS_INFERENCE_TIMEOUT.
func InferenceFromEnv() (Inference, bool) {
	base := os.Getenv("HERODOTOS_INFERENCE_BASE_URL")
	model := os.Getenv("HERODOTOS_INFERENCE_MODEL")
	if base == "" || model == "" {
		return Inference{}, false
	}
	timeout := 120 * time.Second
	if v := os.Getenv("HERODOTOS_INFERENCE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	return Inference{BaseURL: base, Model: model, Timeout: timeout}, true
}
