-- 0006_proposal_outcomes.sql — what adjudication did with each proposed
-- event. Diagnostics only: the committed events are the outcome; this
-- table explains the mapping. "declared" is the orchestrator's own
-- disposition (empty when it gave none); "outcome" is the engine's
-- verdict after comparing the proposal with the final events.

CREATE TABLE proposal_outcomes (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    ref        TEXT NOT NULL,
    author     TEXT NOT NULL,
    title      TEXT NOT NULL,
    declared   TEXT NOT NULL DEFAULT '',
    outcome    TEXT NOT NULL CHECK (outcome IN ('accepted', 'revised', 'rejected', 'unaccounted')),
    reason     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (attempt_id, ref)
);
