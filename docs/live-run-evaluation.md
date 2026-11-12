# Live-run evaluation: twelve steps of "The Four Regions"

A structured evaluation of the first full live run: 12 consecutive
simulation steps of the four-region example world, driven by a real
model through the browser run-control UI. The question is the product
question: does an emergent history stay coherent when regional LLM
historians propose and a deterministic engine commits?

## Setup

- World: `examples/four-regions/world.json` — Aelmark (kingdom),
  Brannholm (march), Corvay (republic), Duskmoor (frontier); 30-day
  steps starting 1 Mar 1204; one soft directive: *"The succession in
  Aelmark must become the central question within the first year."*
- Model: `qwen3.8-flash-next-iq3_s` (a 3-bit-quantized flash-class
  model, 262k context), served by an OpenAI-compatible endpoint over
  LAN, configured only through environment variables.
- Driver: `herodotos serve`; runs started from the web overview via
  `POST /run/start`; steps 8–12 ran as a single background run while
  the browser was closed.
- Steps 1–6 predate the run-control UI; steps 7–12 exercise it fully,
  including a stop/restart test (below).

## Metrics

From the `model_calls` and `attempts` tables over the whole run:

| measure | value |
| --- | --- |
| committed steps | 12 |
| failed attempts | 3 (zero canonical changes; clock never moved wrongly) |
| model calls total (incl. correction retries) | 124 |
| input tokens | 260,367 |
| output tokens | 395,412 |
| total inference time | ~45 min (≈ 2.5–3 min per committed step) |
| truncated replies | 21 (16 before the prompt-budget fix, 5 after) |
| committed events | 59 (of which 12 engine-synthesized directive-review records) |
| events citing canonical causes (steps 5–12) | 31/31 substantive events |
| reports created / delivered | 63 / 39; zero delivered before their earliest step |
| recorded beliefs | 39 across 4 regions (append-and-prune, newest 12 kept) |

Per-step cost after the world had eight steps of history: 6–8 calls,
≈15–23k input tokens, ≈21–27k output tokens, 142–181 s.

Restart recovery was exercised deliberately: the serve process was
killed after step 7 committed and restarted; the clock, world state,
attempt diagnostics, and the idle run panel all came back intact, and
steps 8–12 then ran as one background run. Closing the browser during
that run changed nothing.

## Coherence judgment

Assessment dimensions from the milestone goal, with evidence from the
committed timeline (event ids are canonical).

**Continuity — strong.** Four threads carry across all twelve steps and
thicken rather than drift: Duskmoor's water-oath escalation
(evt-000001 → 6 → 11 → 19 → 27 → 31 → 39 → 41 → 46 → 54 → 56), Aelmark's
loyalty-roll/succession struggle (evt-000002 → 7 → 12 → 16 → 22 → 28 →
32 → 36 → 42 → 47 → 51 → 57), Brannholm's concealed intervention
(evt-000003 → 8 → 13 → 17 → 23 → 29 → 33 → 37 → 43 → 48 → 52 → 58), and
Corvay's credit/covenant machinery (evt-000004 → 9 → 14 → 18 → 24 → 26
→ 34 → 38 → 44 → 49 → 53). Institutions introduced early (ration
office, marcher counsel, Council of Twelve Cantons, well wardens) keep
acting with recognizable positions.

**Causality — strong.** From step 5 (when cause-linking was enforced)
onward, every substantive event names 1–6 canonical causes, and the
links are real: evt-000039 ("Corvay's claim register reaches Duskmoor")
is caused by evt-000024 (the register's creation) and arrives only two
steps later, consistent with its report delivery. No event contradicts
an established one.

**Independent initiative — strong.** Each region pursues its own agenda
rather than orbiting the others: Corvay prices risk and gates credit,
Brannholm conditions covert support on pass tolls and a buffer, Duskmoor
builds oath-based local governance, Aelmark weaponizes ration rolls.
Cross-region influence mostly travels as probes, rumors, and prices —
proposals for one region are never written by another.

**Hidden-information discipline — good.** News arrives on travel time:
Brannholm's probes reach Aelmark as *rumors* (evt-000032) and feed
court accusations only after delivery. Brannholm actively conceals
readiness (evt-000023, 29, 33) and later withdraws observers when
exposure risk rises (evt-000058). Beliefs are recorded as hedged
regional opinions; the knowledge model never leaked a region's private
conditions into another's proposals in this run. One honest gap: not a
single belief was recorded with the `mistaken` modality — the model
under-used the strongest epistemic feature the design offers, so the
"actors act on false beliefs" path remains untested live.

**Directive handling — correct arc.** The orchestrator moved the soft
directive pending → active → fulfilled (evt-000059 records fulfillment
at step 12, within the fictional year of 360 days). It did so by gradual
plausibility — succession genuinely became the central question — not
by contrived fulfillment. The hard-directive contradiction path was
exercised separately (scripted): the world entered the durable pause
with an explanation, history stayed untouched, and the run resumed only
after human direction.

**Plausibility — good with caveats.** The history reads like a dense
political chronicle; conflicts de-escalate realistically (the attestation
compromise, evt-000056). Caveats: the chronicle is drier and more
institutional than colorful; event types drifted into free-form labels
(`court`, `policy`) the schema permits; and the engine's synthetic
`Directive review` records clutter the timeline visually.

## Failure behavior observed

Three attempts failed; all behaved to spec — full diagnostics, zero
canonical changes, no clock movement:

1. Two early failures from model output truncation before the
   correction-steering fix.
2. One step failed when a verbose region exhausted both correction
   retries mid-run. The background run stopped at the failure with the
   error on the panel; a re-run of that step boundary committed cleanly.

That failure mode drove the one code change made during the live phase:
explicit token budgets in the agent prompts (commit 38461c0). After
that, truncation frequency dropped from ~40% of replies to ~12% and no
step failed — bounded retries absorbed the rest.

## Verdict

Twelve steps produced a causally linked, epistemically disciplined
history with four independent regional arcs, one correctly fulfilled
directive, and zero canonical corruption across three failures and one
process restart. The premise — agents propose, the engine commits,
coherence survives — is validated at this scale.

Known next steps: the model's verbosity remains the main operational
risk (a bigger budget of correction retries, or an even tighter reply
schema, would help); encourage `mistaken` beliefs so in-world error
becomes a story driver; consider making directive-review records
display-only; and constrain or normalize event `type` values.
