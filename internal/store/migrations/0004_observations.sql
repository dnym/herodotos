-- 0004_observations.sql — visibility is separate from geography. An
-- event's region_ids say where it is relevant (filtering, inspection);
-- observations say which regions' agents actually witnessed it, and
-- with which account. Being affected by a covert act is not observing
-- it: a target region learns only by observation or report delivery.
-- account NULL means the full canonical prose; a non-NULL account is
-- the limited version that region perceived or was told.

CREATE TABLE observations (
    event_id  TEXT NOT NULL REFERENCES events(id),
    region_id TEXT NOT NULL REFERENCES regions(id),
    account   TEXT,
    PRIMARY KEY (event_id, region_id)
);

CREATE INDEX idx_observations_region ON observations(region_id);

-- Earlier histories recorded no observers. Their only visibility rule
-- was region_ids membership, so existing events keep exactly that
-- access; new events record observers explicitly.
INSERT INTO observations(event_id, region_id)
SELECT e.id, j.value FROM events e, json_each(e.region_ids) j
WHERE j.value IN (SELECT id FROM regions);

-- A report may carry a limited account instead of the full prose.
ALTER TABLE reports ADD COLUMN account TEXT;
