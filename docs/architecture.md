# Herodotos architecture

This document describes the architecture as built, and records deliberate
deviations from the original milestone plan where the implementation
chose a different route. It assumes no access to any planning document;
everything needed is here or in the code.

## Layers

```
cmd/herodotos        CLI: init, step, run, inspect, resume, serve
internal/domain      canonical types, stable ids (no storage or HTTP deps)
internal/store       SQLite schema, migrations, snapshots, atomic commit
internal/knowledge   visibility policy: what each agent may know
internal/agents      role prompts, strict JSON parsing, structural validation
internal/sim         the step engine: propose → adjudicate → commit
internal/inference   model transport: OpenAI-compatible client + scripted fake
internal/web         HTML overview, run control, export
```

The simulation engine depends on neither the CLI nor the web layer. The
web layer and the CLI are both thin callers of `sim.Engine.RunStep`.

## Authority model

- Canonical structured state (regions, entities, relationships, events,
  reports, beliefs, memory) and committed events are authoritative.
  Prose, reference text, and memory summaries only describe them.
- Agents propose; they never write storage. Only the simulation engine
  commits, in one short database transaction per step. No model call
  ever happens inside a write transaction.
- Three epistemic layers are kept distinct: canonical truth,
  agent-accessible knowledge (visibility-scoped views), and in-world
  beliefs (recorded opinions that may be wrong). Reports carry an
  earliest-delivery step derived from travel days; a report is
  invisible to its recipient region before that step, and one region's
  private information never enters another region's view.
- Geography is not access. An event's `region_ids` list where it is
  relevant (the origin and every region whose state or entities it
  touches) and drive filtering and inspection only. Agent access comes
  from two persistent records: `observations` (the origin, plus the
  regions the final event declares as `observers`, i.e. whose people
  witness it as it happens) and delivered `reports`. Both may carry a
  limited `account` that replaces the event's title and prose for
  that region, so a covert act's target can be affected now, told a
  partial story later, and never shown the private canonical prose.
  Recent-history context, report notes, cause validation, reaction
  premises, and history recall all apply this one policy. An event
  with no regional origin must name at least one observer. Worlds
  created before this rule existed keep their old access: the
  migration records every listed region as an observer of the events
  already committed.
- The access record outlives the prompt's eight-step recent window: an
  old event stays a valid cause for regions that witnessed or received
  it, and a logographer may ask to `recall` up to three event ids or
  keyword queries, answered in the same single follow-up call as
  `consult` from the region's own record (at most eight entries,
  matched against the version the region knows).
- News timing is a code-enforced baseline, not a transport
  simulation. A report's earliest step must be at least one step after
  the event and no earlier than the shortest connection route from any
  witness (origin or observer) allows: one step per started step length
  of travel days. A region with no known route is unreachable by
  ordinary news. Beating the baseline needs an explicit `expedited`
  mechanism per target, which the orchestrator adjudicates and the
  report records. Logographer prompts list each reachable region with
  its route length and earliest delivery step.
- Failed attempts keep full diagnostics (proposals, model calls,
  tokens, errors) but produce zero canonical changes and never move
  the clock. Commitment is guarded by a base revision; a stale
  snapshot yields `ErrStaleRevision`, not a partial write.

## Prompt budgets

Prompts stay structured (`agents.Prompt`: a head, prioritized sections,
a footer) until the final assembly, which happens once per model call
after every late addition — a reaction request, consulted texts,
correction feedback — is known. Assembly drops optional sections
lowest priority first and never touches mandatory ones:

- logographer: the head (region conditions, memory), directives, and
  binding references;
- orchestrator: directives, binding references, the proposals, the
  detected interactions, the canonical entities and relationships the
  proposals touch, regions, and connections.

If the mandatory material plus the additions cannot fit
`MaxPromptBytes`, assembly returns `agents.BudgetError` and the attempt
fails before any inference call, with zero canonical change. Correction
feedback is capped so a verbose validator message cannot itself break
the budget.

## The durable pause

When the orchestrator decides a hard directive cannot be satisfied
without contradicting canonical history, it replies with a `pause`
object. That reply does two things:

1. The attempt fails (status `failed`, `pause_reason` recorded) — so
   the step produces zero canonical changes, like any failure.
2. The world enters a durable paused state: a `world_paused` flag in
   the `meta` table holding the human-readable reason.

The pause is world state, not process state. It survives process
restarts and database reopen. `RunStep` refuses to start any attempt
while it is set (`sim.ErrPaused`), and every surface shows the reason:
the overview's run panel, `herodotos inspect`, and the CLI run loop.
Recovery is explicit: the human edits or adds directives from the
overview (or the database by other means), then resumes — via the web
panel or `herodotos resume`. The next attempt then adjudicates afresh
against the corrected direction.

A second, unrelated kind of pause is the *run pause*: stopping a
background run (see below) at the next step boundary. It touches
nothing canonical and is process-local by design.

## Run control (web)

- Steps run in a server-managed background run, not in the HTTP
  request. Starting a run returns immediately; closing the browser
  never terminates a step. The run goroutine uses a server-lifetime
  context, so shutdown cancels it but client disconnect cannot.
- One run at a time; a second start is refused while one is active.
- Runs execute N steps or run until stopped; the only pause point is
  the step boundary, which preserves step atomicity. A run stops on a
  step failure and its result/error is shown on the panel.
- The panel partial (`/run/panel`) is re-fetched by vendored HTMX every
  four seconds only while a run is active or the world is paused.
- CSRF posture for a local, credential-free tool: every mutation
  endpoint is wrapped in a same-origin check — a carried `Origin` must
  match the `Host` (403 otherwise); no CORS headers are ever set;
  Origin-less requests (curl, forms in the same tab navigation) pass.

## Directives

Directives are human creative direction with an explicit hard/soft
classification and a status (`pending`, `active`, `fulfilled`,
`conflicting`). The orchestrator may update statuses; agents may never
touch directives at all. Humans can reword directives and add new ones
from the overview. A hard directive that cannot be met is the pause
trigger — the engine does not try to detect semantic contradictions
itself; that judgment is the orchestrator's, and the human resolves it.

## Export

`GET /export` serializes the entire canonical world (state, timeline,
reports, beliefs, memories, directives, references, meta) as one JSON
document with snake_case keys. Attempt diagnostics are deliberately
excluded: they are engineering traces, not world content.

## Deviations and deferrals

The original plan listed a few features that are deliberately not built
the way they were sketched:

- **Ongoing activities as a first-class table.** Deferred. The intent —
  keeping unresolved intentions and future outcomes visible across
  steps — is currently served by two shipped mechanisms: per-region
  memory `open_threads` (the logographer maintains its region's live
  intentions each step) and beliefs with modalities (`known`,
  `uncertain`, `mistaken`). A dedicated activities table should be
  revisited only if evaluation shows `open_threads` losing threads
  that matter; adding it earlier risks the engine hard-coding
  civilization semantics it must not own.
- **Reference retrieval is keyword-scored, not semantic.** References
  follow the skill pattern: each has a one-line `description` always
  shown to scoped agents, and its body loads only when needed. Binding
  documents always appear in full (logographer prompts and the
  orchestrator's BINDING REFERENCES section); advisory documents are
  auto-retrieved by deterministic keyword scoring against the region's
  current situation (region name, attributes, entity names, memory,
  recent events — top 3 bodies per prompt), and otherwise appear as an
  id/title/description catalog. A historian may also answer with a
  "consult" field naming up to 3 cataloged ids: the engine spends one
  bounded follow-up call re-asking that region with those full texts
  (no second round; unknown or out-of-scope ids are dropped; the round
  counts against `MaxModelCallsPerStep`). No embeddings and no
  inference are spent on automatic retrieval; if keyword matching
  proves too coarse, the view builder
  (`internal/knowledge.classifyReferences`) is the seam where
  something smarter would slot in.
- **Belief storage is append-and-prune, not merge.** Each logographer
  may record up to four new or changed beliefs per reply; the store
  keeps the twelve newest per region. There is no belief revision
  ledger: beliefs are in-world opinions, and recency is what prompts
  need.

## Testing posture

The default test suite is hermetic: scripted model replies
(`testdata/model-responses/`), temporary SQLite databases, and no
network. Live model experiments are opt-in through environment
variables (`HERODOTOS_INFERENCE_BASE_URL`, `HERODOTOS_INFERENCE_MODEL`,
`HERODOTOS_INFERENCE_TIMEOUT`) and are never part of `go test ./...`.
Concurrency-sensitive code (background runs, pause coordination) is
tested under `-race`.
