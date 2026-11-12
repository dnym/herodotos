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
}
