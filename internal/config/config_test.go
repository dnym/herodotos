package config

import "testing"

func TestBudgetsFromEnv(t *testing.T) {
	t.Setenv("HERODOTOS_BUDGET_MAX_OUTPUT_TOKENS", "9000")
	t.Setenv("HERODOTOS_BUDGET_MAX_CORRECTION_RETRIES", "bogus")
	t.Setenv("HERODOTOS_BUDGET_MAX_REACTION_ROUNDS", "-1")

	b := BudgetsFromEnv()
	if b.MaxOutputTokens != 9000 {
		t.Errorf("override ignored: %+v", b)
	}
	def := DefaultBudgets()
	if b.MaxCorrectionRetries != def.MaxCorrectionRetries ||
		b.MaxReactionRounds != def.MaxReactionRounds {
		t.Errorf("invalid overrides must be ignored: %+v", b)
	}
	if def.MaxOutputTokensOrchestr <= def.MaxOutputTokens {
		t.Error("orchestrator ceiling must exceed the regional ceiling")
	}

	// An explicit zero is a deliberate choice, not a mistake.
	t.Setenv("HERODOTOS_BUDGET_MAX_REACTION_ROUNDS", "0")
	t.Setenv("HERODOTOS_BUDGET_MAX_CORRECTION_RETRIES", "0")
	b = BudgetsFromEnv()
	if b.MaxReactionRounds != 0 || b.MaxCorrectionRetries != 0 {
		t.Errorf("zero overrides must be honoured: %+v", b)
	}
}
