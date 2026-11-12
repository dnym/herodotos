-- 0001_init.sql — canonical world state, committed history, and attempt
-- diagnostics. Canonical tables are authoritative; attempt tables hold
-- diagnostics only and never contribute committed facts.

PRAGMA foreign_keys = ON;

-- World metadata, clock, and counters.
-- Keys: world_id, world_name, start_date, step_days, clock_step, revision,
-- next-event-id, next-report-id, next-belief-id, next-attempt-id,
-- next-proposal-id, next-reference-id, next-directive-id, next-relationship-id.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Geography. Region identities are stable and independent of agents.
CREATE TABLE regions (
    id     TEXT PRIMARY KEY,
    name   TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'dormant')),
    attrs  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attrs))
);

-- Travel connections; travel_days is the baseline delay for information.
CREATE TABLE connections (
    region_a    TEXT NOT NULL REFERENCES regions(id),
    region_b    TEXT NOT NULL REFERENCES regions(id),
    travel_days INTEGER NOT NULL CHECK (travel_days >= 1),
    PRIMARY KEY (region_a, region_b),
    CHECK (region_a < region_b)
);

-- Historical entities with identities persistent across changes.
CREATE TABLE entities (
    id        TEXT PRIMARY KEY,
    kind      TEXT NOT NULL CHECK (kind IN ('person', 'group', 'org', 'settlement', 'polity')),
    name      TEXT NOT NULL,
    region_id TEXT REFERENCES regions(id),
    status    TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'destroyed')),
    attrs     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attrs))
);

CREATE TABLE relationships (
    id         TEXT PRIMARY KEY,
    source_id  TEXT NOT NULL REFERENCES entities(id),
    target_id  TEXT NOT NULL REFERENCES entities(id),
    kind       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'ended')),
    attrs      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attrs)),
    CHECK (source_id <> target_id)
);

-- Human creative direction, with explicit hard/soft classification.
CREATE TABLE directives (
    id     TEXT PRIMARY KEY,
    text   TEXT NOT NULL,
    hard   INTEGER NOT NULL DEFAULT 0 CHECK (hard IN (0, 1)),
    status TEXT NOT NULL DEFAULT 'pending'
           CHECK (status IN ('pending', 'active', 'fulfilled', 'conflicting')),
    notes  TEXT
);

-- Committed history. Only fully committed steps appear here.
CREATE TABLE steps (
    step          INTEGER PRIMARY KEY,
    committed_utc TEXT NOT NULL,
    summary       TEXT
);

CREATE TABLE events (
    id         TEXT PRIMARY KEY,
    step       INTEGER NOT NULL REFERENCES steps(step),
    type       TEXT NOT NULL,
    title      TEXT NOT NULL,
    prose      TEXT NOT NULL,
    region_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(region_ids)),
    entity_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(entity_ids)),
    cause_ids  TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(cause_ids))
);

CREATE INDEX idx_events_step ON events(step);

-- Structured state transitions tied to the events that authorized them.
CREATE TABLE state_changes (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id  TEXT NOT NULL REFERENCES events(id),
    object    TEXT NOT NULL CHECK (object IN ('region', 'entity', 'relationship', 'directive')),
    object_id TEXT NOT NULL,
    op        TEXT NOT NULL CHECK (op IN ('set_attrs', 'upsert', 'set_status')),
    patch     TEXT NOT NULL CHECK (json_valid(patch)),
    before    TEXT CHECK (before IS NULL OR json_valid(before))
);

CREATE INDEX idx_state_changes_object ON state_changes(object, object_id);

-- Knowledge propagation between regions with explicit delivery times.
CREATE TABLE reports (
    id               TEXT PRIMARY KEY,
    event_id         TEXT NOT NULL REFERENCES events(id),
    target_region_id TEXT NOT NULL REFERENCES regions(id),
    earliest_step    INTEGER NOT NULL,
    delivered_step   INTEGER
);

CREATE INDEX idx_reports_due ON reports(target_region_id, earliest_step);

-- In-world beliefs, kept distinct from canonical truth and from
-- agent-accessible information.
CREATE TABLE beliefs (
    id           TEXT PRIMARY KEY,
    region_id    TEXT NOT NULL REFERENCES regions(id),
    subject_id   TEXT REFERENCES entities(id),
    modality     TEXT NOT NULL CHECK (modality IN ('known', 'uncertain', 'mistaken')),
    statement    TEXT NOT NULL,
    as_of_step   INTEGER NOT NULL
);

-- Compact persistent agent memories; never override canonical facts.
CREATE TABLE memories (
    region_id    TEXT PRIMARY KEY REFERENCES regions(id),
    summary      TEXT NOT NULL,
    open_threads TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(open_threads)),
    updated_step INTEGER NOT NULL
);

-- Reference documents; binding rules are explicitly designated.
CREATE TABLE reference_docs (
    id        TEXT PRIMARY KEY,
    scope     TEXT NOT NULL CHECK (scope IN ('world', 'region')),
    region_id TEXT REFERENCES regions(id),
    binding   INTEGER NOT NULL DEFAULT 0 CHECK (binding IN (0, 1)),
    title     TEXT NOT NULL,
    body      TEXT NOT NULL,
    keywords  TEXT NOT NULL DEFAULT ''
);

-- Attempt diagnostics: kept separate from canonical history so failed
-- attempts can be investigated without polluting committed facts.
CREATE TABLE attempts (
    id          TEXT PRIMARY KEY,
    step        INTEGER NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('running', 'committed', 'failed', 'interrupted')),
    started_utc TEXT NOT NULL,
    ended_utc   TEXT,
    error       TEXT
);

CREATE TABLE model_calls (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id    TEXT NOT NULL REFERENCES attempts(id),
    role          TEXT NOT NULL,
    model         TEXT NOT NULL,
    prompt_sha256 TEXT NOT NULL,
    request_json  TEXT,
    response_json TEXT,
    input_tokens  INTEGER,
    output_tokens INTEGER,
    duration_ms   INTEGER,
    error         TEXT
);

CREATE TABLE proposals (
    id              TEXT PRIMARY KEY,
    attempt_id      TEXT NOT NULL REFERENCES attempts(id),
    author          TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('development', 'reaction', 'none')),
    prose           TEXT NOT NULL,
    changes         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(changes)),
    affects_regions TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(affects_regions)),
    affects_entities TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(affects_entities)),
    causes          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(causes)),
    delivery        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(delivery))
);

CREATE INDEX idx_proposals_attempt ON proposals(attempt_id);
