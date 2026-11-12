# Scripted model-response fixtures

Files in this directory replay deterministic agent replies for offline
runs and tests. Each file is a JSON array of objects:

```json
{ "role": "logographer:<region-id> | orchestrator", "text": "<reply>" }
```

Replies are consumed strictly in array order. The engine calls regions
in ascending id order (active regions only), then the orchestrator, and
correction retries repeat the same role — so fixture order must match
that call sequence. A mismatched role fails the step, and the step's
attempt keeps its diagnostics without committing anything.

`four-regions-step1.json` drives one complete step of
`examples/four-regions/world.json`:

    herodotos init --world examples/four-regions/world.json --db world.db
    herodotos step --db world.db --scripted testdata/model-responses/four-regions-step1.json
