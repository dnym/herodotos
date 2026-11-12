-- 0003_references.sql — reference documents become skill-style: a
-- one-line description is always shown to scoped agents, and the body
-- loads only when keyword retrieval matches or the agent asks. The
-- enabled flag lets a human park a document without deleting it.

ALTER TABLE reference_docs ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE reference_docs ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1));
