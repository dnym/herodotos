-- 0002_run_control.sql — durable explanation for a halted step. The pause
-- itself is a meta-table flag (key 'world_paused'); this column records per
-- attempt why its step was stopped, so the UI can explain and recover it.

ALTER TABLE attempts ADD COLUMN pause_reason TEXT;
