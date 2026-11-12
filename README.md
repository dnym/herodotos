# herodotos

An LLM-driven emergent worldbuilding simulator. Regional historian
agents — **logographers**, after the early Greek writers who each told
the story of their own region — propose developments for their region;
one global orchestrator adjudicates the proposals into a single shared
history; a deterministic simulation engine validates and commits the
outcome to SQLite. Agents propose. Only the engine commits.

## The shape of a step

1. The engine snapshots the committed world at the current clock step.
2. Each active region's logographer receives a visibility-scoped view —
   its region's canonical conditions, delivered reports, recorded
   beliefs, its own memory — and proposes events as strict JSON.
3. The engine detects interactions structurally (shared write targets,
   shared participants, cross-border actions) and asks the orchestrator
   to adjudicate everything into one shared history. Reaction rounds
   and correction retries are bounded.
4. The engine validates every change (identities, regional authority,
   delivery times), then commits events, state changes, delayed
   reports, recorded beliefs, and memory updates in one short database
   transaction. Beliefs are in-world opinions that may be wrong; each
   region's set is pruned to its twelve newest.

Canonical state and committed events are authoritative; prose, memory,
and beliefs only describe them. News reaches other regions only at
travel-appropriate steps, and a failed attempt keeps its full
diagnostics while changing nothing.

## Quick start

```sh
go build -o herodotos ./cmd/herodotos

herodotos init --world examples/four-regions/world.json --db world.db

# A live run needs an OpenAI-compatible inference endpoint:
export HERODOTOS_INFERENCE_BASE_URL=https://your-host/v1
export HERODOTOS_INFERENCE_MODEL=your-model
herodotos step --db world.db        # one step
herodotos run 6 --db world.db       # six consecutive steps
herodotos serve --db world.db       # web overview on http://127.0.0.1:8080

# Or replay deterministic replies with no model server at all:
herodotos step --db world.db --scripted testdata/model-responses/four-regions-step1.json
herodotos inspect --db world.db
herodotos resume --db world.db      # lift a durable world pause
```

`serve` shows the world overview, timeline, per-attempt diagnostics
(proposals, model calls, tokens, errors), and run control: start a run
of one, N, or unlimited steps from the browser; steps execute in a
server-managed background run, so closing the tab never terminates one.
A run can be paused only at a step boundary, which keeps every step
atomic. When the orchestrator halts the world on a contradiction, the
pause is durable world state: it survives restarts, blocks further
steps, and the overview explains it until you resolve the conflict and
resume — from the browser or `herodotos resume`. Directive text can be
edited and new directives added from the overview (the historians
never change directives themselves), and `/export` downloads the
entire canonical world as one JSON document. Mutation endpoints reject
cross-origin requests. The prose UI is set in vendored EB Garamond
(SIL OFL 1.1); no external assets are fetched at runtime. Exactly one
process may hold a world database at a time; others exit with a clear
lock error.

## Repository layout

    cmd/herodotos/        CLI: init, step, run, inspect, serve
    internal/domain/      canonical entities and identifiers
    internal/store/       SQLite schema, migrations, snapshots, atomic commits
    internal/knowledge/   visibility policy: what each agent may know
    internal/agents/      role prompts, strict parsing, structural validation
    internal/sim/         the step engine: propose → adjudicate → commit
    internal/inference/   model transport: OpenAI-compatible + scripted fake
    internal/web/         HTML overview and diagnostics pages
    examples/             world definitions
    testdata/             scripted model replies

## Design invariants

- One short write transaction per step; no model calls inside a
  transaction; a step commits against a base revision or not at all.
- Failed attempts: full diagnostics, zero canonical changes, no clock
  movement, no double commits after retries.
- Canonical truth, agent-accessible knowledge, and in-world beliefs are
  three different things; reports are unavailable before their delivery
  time; one region's private information never leaks into another's.
- Structural invariants are enforced by code; semantic plausibility is
  the orchestrator's job. The engine is not a hand-coded civilization
  simulator.
- All budgets are explicit and env-tunable: two reaction rounds, two
  correction retries, twenty model calls per step, per-role output
  ceilings (see `internal/config` and `herodotos --help`).

## Development

```sh
go test ./...   # hermetic: scripted replies, temporary SQLite databases
go vet ./...
```
