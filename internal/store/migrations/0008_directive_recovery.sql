-- 0008_directive_recovery.sql — a conflict over a hard directive can be
-- resolved by weakening or withdrawing it, not only by rewording it.
-- 'withdrawn' directives stay on record but no agent sees them. Every
-- human change to a directive is logged, so the creative direction a
-- step was adjudicated under can be reconstructed.

CREATE TABLE directives_new (
    id     TEXT PRIMARY KEY,
    text   TEXT NOT NULL,
    hard   INTEGER NOT NULL DEFAULT 0 CHECK (hard IN (0, 1)),
    status TEXT NOT NULL DEFAULT 'pending'
           CHECK (status IN ('pending', 'active', 'fulfilled', 'conflicting', 'withdrawn')),
    notes  TEXT
);
INSERT INTO directives_new(id, text, hard, status, notes)
SELECT id, text, hard, status, notes FROM directives;
DROP TABLE directives;
ALTER TABLE directives_new RENAME TO directives;

CREATE TABLE directive_log (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    directive_id TEXT NOT NULL,
    at_step      INTEGER NOT NULL, -- committed clock when the change was made
    change       TEXT NOT NULL CHECK (change IN ('created', 'reworded', 'hardened', 'softened', 'withdrawn', 'restored')),
    before       TEXT,
    after        TEXT,
    utc          TEXT NOT NULL
);

CREATE INDEX idx_directive_log_directive ON directive_log(directive_id);
