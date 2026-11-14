-- 0007_diagnostics_detail.sql — failures and calls explain themselves
-- after a restart. attempts.error keeps the failing stage ("region x
-- proposal failed", "commit"); detail keeps the underlying cause
-- (validation, budget, storage), so the attempt page says what to do
-- next. Model calls record the effective request settings, so
-- experiments stay comparable.

ALTER TABLE attempts ADD COLUMN detail TEXT;
ALTER TABLE model_calls ADD COLUMN temperature REAL;
ALTER TABLE model_calls ADD COLUMN max_tokens INTEGER;
ALTER TABLE model_calls ADD COLUMN prompt_bytes INTEGER;
ALTER TABLE model_calls ADD COLUMN prompt_budget INTEGER;
