// Package config centralizes budgets and runtime configuration. Limits
// exist so a step cannot run indefinitely, exhaust tokens, or negotiate
// forever.
package config

import (
	"os"
	"strconv"
	"time"
)

// Budgets bounds one simulation step. Defaults follow the MVP policy:
// two reaction/revision rounds and two correction retries. Output is
// budgeted per role because the orchestrator re-emits the merged
// history and needs the larger ceiling. The ceilings are headroom over
// what prompt-disciplined replies actually produce (live runs showed
// logographer replies of 1-2.5k tokens): they guard against runaway
// verbosity, not against normal output. Every field can be overridden
// with an environment variable.
type Budgets struct {
	MaxReactionRounds       int
	MaxCorrectionRetries    int
	MaxModelCallsPerStep    int
	MaxOutputTokens         int
	MaxOutputTokensOrchestr int
	MaxPromptBytes          int
}

// DefaultBudgets returns the shipped defaults.
func DefaultBudgets() Budgets {
	return Budgets{
		MaxReactionRounds:       2,
		MaxCorrectionRetries:    2,
		MaxModelCallsPerStep:    20,
		MaxOutputTokens:         6000,
		MaxOutputTokensOrchestr: 8000,
		MaxPromptBytes:          120_000,
	}
}

// BudgetsFromEnv returns the defaults with any HERODOTOS_BUDGET_*
// overrides applied: each named after the field in SCREAMING_SNAKE,
// e.g. HERODOTOS_BUDGET_MAX_OUTPUT_TOKENS=8000. Reaction rounds and
// correction retries accept an explicit 0 (deliberately switching the
// mechanism off); for every other knob a non-numeric or non-positive
// override is ignored.
func BudgetsFromEnv() Budgets {
	b := DefaultBudgets()
	envIntNonNeg("HERODOTOS_BUDGET_MAX_REACTION_ROUNDS", &b.MaxReactionRounds)
	envIntNonNeg("HERODOTOS_BUDGET_MAX_CORRECTION_RETRIES", &b.MaxCorrectionRetries)
	envInt("HERODOTOS_BUDGET_MAX_MODEL_CALLS_PER_STEP", &b.MaxModelCallsPerStep)
	envInt("HERODOTOS_BUDGET_MAX_OUTPUT_TOKENS", &b.MaxOutputTokens)
	envInt("HERODOTOS_BUDGET_MAX_OUTPUT_TOKENS_ORCHESTR", &b.MaxOutputTokensOrchestr)
	envInt("HERODOTOS_BUDGET_MAX_PROMPT_BYTES", &b.MaxPromptBytes)
	return b
}

func envInt(key string, into *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*into = n
		}
	}
}

func envIntNonNeg(key string, into *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			*into = n
		}
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
