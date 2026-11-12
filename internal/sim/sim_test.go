package sim

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dnym/herodotos/internal/agents"
	"github.com/dnym/herodotos/internal/config"
	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/inference"
	"github.com/dnym/herodotos/internal/knowledge"
	"github.com/dnym/herodotos/internal/store"
)

func testWorld() *store.WorldSpec {
	w := &store.WorldSpec{Name: "Test World", StartDate: "1000-06-01", StepDays: 7}
	w.Regions = []struct {
		ID    domain.ID      `json:"id"`
		Name  string         `json:"name"`
		Attrs map[string]any `json:"attrs,omitempty"`
	}{
		{ID: "king-a", Name: "Kingdom A", Attrs: map[string]any{"unrest": "low"}},
		{ID: "prin-b", Name: "Principality B", Attrs: map[string]any{"ambition": "expansionist"}},
	}
	w.Connections = []struct {
		A         domain.ID `json:"a"`
		B         domain.ID `json:"b"`
		TravelDay int       `json:"travel_days"`
	}{
		{A: "king-a", B: "prin-b", TravelDay: 4},
	}
	w.Entities = []struct {
		ID       domain.ID      `json:"id"`
		Kind     string         `json:"kind"`
		Name     string         `json:"name"`
		RegionID domain.ID      `json:"region_id,omitempty"`
		Attrs    map[string]any `json:"attrs,omitempty"`
	}{
		{ID: "q-elinor", Kind: "person", Name: "Queen Elinor", RegionID: "king-a"},
	}
	w.Memories = map[domain.ID]struct {
		Summary     string   `json:"summary"`
		OpenThreads []string `json:"open_threads,omitempty"`
	}{
		"king-a": {Summary: "A fragile monarchy."},
		"prin-b": {Summary: "A neighbor with ambitions."},
	}
	return w
}

func setup(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.InitWorld(ctx, testWorld()); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

const kingReply = `{"prose":"Unrest spreads.","events":[{"type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"],"delivery":{"prin-b":3}}],"memory":{"summary":"Rioting began.","open_threads":["queen considers flight"]}}`

const prinQuiet = `{"prose":"A quiet week.","events":[],"memory":{"summary":"Watching the kingdom nervously."}}`

const orchAccept = `{"notes":"Accepted the regional development.","events":[{"origin":"king-a","type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"],"delivery":{"prin-b":3}}]}`

const kingTwoProposals = `{"prose":"Unrest spreads; the queen calls a council.","events":[{"type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"]},{"type":"development","title":"Emergency council named","prose":"The queen names a regency council.","changes":[{"object":"entity","object_id":"council-regency","op":"upsert","patch":{"kind":"group","name":"Regency Council","region_id":"king-a"}}]}],"memory":{"summary":"Rioting began; a regency council was named."}}`

const orchPartial = `{"notes":"The riots are accepted; the council is premature.","events":[{"origin":"king-a","type":"development","title":"Rioting in the capital","prose":"Crowds demand grain.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"high"}}],"affects_entities":["q-elinor"]},{"origin":"orchestrator","type":"development","title":"Court whispers of regency","prose":"Nameless rumors circulate; no council is named.","changes":[]}]}`

const orchReplaceAll = `{"notes":"Regional account rejected; a quieter world.","events":[{"origin":"orchestrator","type":"development","title":"A tense silence","prose":"The capital holds its breath; no riots come.","changes":[{"object":"region","object_id":"king-a","op":"set_attrs","patch":{"unrest":"tense"}}],"affects_entities":["q-elinor"]}]}`

// A region whose proposals were all rejected must not carry their
// invented outcomes into persistent memory.
func TestRejectedProposalsDoNotRewriteMemory(t *testing.T) {
	db, ctx := setup(t)
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchReplaceAll},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatalf("run step: %v", err)
	}
	snap, _ := db.LoadSnapshot(ctx)
	mem := snap.Memories["king-a"]
	if mem.Summary != "A fragile monarchy." || mem.UpdatedStep != 0 {
		t.Errorf("rejected outcomes entered memory: %+v", mem)
	}
	if snap.Regions["king-a"].Attrs["unrest"] != "tense" {
		t.Errorf("orchestrator event should still commit: %v", snap.Regions["king-a"].Attrs)
	}
	// The next step must not be fed the invented history: the prompt
	// carries the old memory, and the rejected riot never appears.
	prompt := nextStepPrompt(t, db, ctx, "king-a")
	if strings.Contains(prompt, "Rioting began") {
		t.Error("next-step prompt contains invented history")
	}
	if !strings.Contains(prompt, "A fragile monarchy.") {
		t.Error("next-step prompt lost the established memory")
	}
}

// Partial acceptance keeps memory: some of the region's account became
// history, and it is not the engine's job to split prose.
func TestPartialAcceptanceKeepsMemory(t *testing.T) {
	db, ctx := setup(t)
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingTwoProposals},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchPartial},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	res, err := e.RunStep(ctx)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if len(res.EventIDs) != 2 {
		t.Fatalf("expected one accepted + one orchestrator event: %+v", res)
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Memories["king-a"].UpdatedStep != 1 ||
		!strings.Contains(snap.Memories["king-a"].Summary, "Rioting began") {
		t.Errorf("partial acceptance should keep the memory update: %+v", snap.Memories["king-a"])
	}
}

func TestEntityMoveVisibleToDestinationRegion(t *testing.T) {
	db, ctx := setup(t)
	// The queen leaves Aelmark for the principality; the engine, not a
	// logographer, adjudicates the move.
	orchMove := `{"notes":"The queen flees north.","events":[{"origin":"orchestrator","type":"movement","title":"The queen crosses the march","prose":"Elinor leaves the capital for Brannholm.","changes":[{"object":"entity","object_id":"q-elinor","op":"upsert","patch":{"region_id":"prin-b"}}]}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchMove},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatalf("run step: %v", err)
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Entities["q-elinor"].RegionID != "prin-b" {
		t.Fatalf("canonical location unchanged: %+v", snap.Entities["q-elinor"])
	}
	// Next step: the destination historian sees her, the origin one does not.
	if p := nextStepPrompt(t, db, ctx, "prin-b"); !strings.Contains(p, "Queen Elinor") {
		t.Error("moved entity not visible to destination region")
	}
	if p := nextStepPrompt(t, db, ctx, "king-a"); strings.Contains(p, "Queen Elinor") {
		t.Error("moved entity still visible to origin region")
	}
}

func nextStepPrompt(t *testing.T, db *store.DB, ctx context.Context, regionID string) string {
	t.Helper()
	snap, err := db.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := knowledge.BuildLogographerView(ctx, snap, domain.ID(regionID), db)
	if err != nil {
		t.Fatal(err)
	}
	return agents.RenderLogographerPrompt(snap.Meta, snap.Meta.ClockStep, view, 1<<20)
}

func TestFullStepCommitsOneSharedHistory(t *testing.T) {
	db, ctx := setup(t)
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	res, err := e.RunStep(ctx)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.Step != 1 || len(res.EventIDs) != 1 {
		t.Fatalf("result: %+v", res)
	}
	if res.ModelCalls != 3 || res.Proposals != 1 {
		t.Fatalf("counters: %+v", res)
	}

	snap, _ := db.LoadSnapshot(ctx)
	if snap.Regions["king-a"].Attrs["unrest"] != "high" {
		t.Fatalf("state: %v", snap.Regions["king-a"].Attrs)
	}
	if snap.Memories["prin-b"].Summary == "" || snap.Memories["prin-b"].UpdatedStep != 1 {
		t.Fatalf("memory: %+v", snap.Memories["prin-b"])
	}
	reports, _ := db.ListReports(ctx)
	if len(reports) != 1 || reports[0].Report.EarliestStep != 3 || reports[0].Report.DeliveredStep != 0 {
		t.Fatalf("reports: %+v", reports)
	}
	attempts, _ := db.ListAttempts(ctx, 5)
	if len(attempts) != 1 || attempts[0].Status != "committed" {
		t.Fatalf("attempts: %+v", attempts)
	}
	proposals, _ := db.ListProposals(ctx, res.AttemptID)
	if len(proposals) != 2 {
		t.Fatalf("proposals recorded: %d", len(proposals))
	}
	calls, _ := db.ListModelCalls(ctx, res.AttemptID)
	if len(calls) != 3 {
		t.Fatalf("model calls recorded: %d", len(calls))
	}
}

func TestFinalEventRequiresCausesAfterFirstStep(t *testing.T) {
	db, ctx := setup(t)
	step1 := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: step1, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatal(err)
	}

	noCause := `{"notes":"linked","events":[{"origin":"prin-b","type":"development","title":"Mustering","prose":"Drums on the border.","changes":[]}]}`
	withCause := `{"notes":"linked","events":[{"origin":"prin-b","type":"development","title":"Mustering","prose":"Drums on the border.","changes":[],"causes":["evt-000001"]}]}`
	step2 := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: noCause},
		{Role: "orchestrator", Text: withCause},
	}}
	e2 := &Engine{DB: db, Client: step2, ModelName: "fake"}
	if _, err := e2.RunStep(ctx); err != nil {
		t.Fatalf("empty causes should have been corrected, not fatal: %v", err)
	}
	calls := step2.Calls()
	if len(calls) != 4 {
		t.Fatalf("want 4 calls, got %d", len(calls))
	}
	if !strings.Contains(calls[3].Prompt, "cites no causes") {
		t.Fatal("causality correction missing from prompt")
	}
	events, _ := db.ListEvents(ctx, store.EventQuery{})
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	linked, err := db.GetEvent(ctx, "evt-000002")
	if err != nil {
		t.Fatal(err)
	}
	if len(linked.CauseIDs) != 1 || linked.CauseIDs[0] != "evt-000001" {
		t.Fatalf("causes not committed: %v", linked.CauseIDs)
	}
}

func TestRegionAuthorityCorrection(t *testing.T) {
	db, ctx := setup(t)
	bad := `{"prose":"overreach","events":[{"type":"development","title":"Alarm next door","prose":"p","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"alarm":"high"}}]}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: bad},
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	res, err := e.RunStep(ctx)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ModelCalls != 4 {
		t.Fatalf("expected 4 calls with one correction, got %d", res.ModelCalls)
	}
	// The correction prompt must explain the rejection.
	calls := fake.Calls()
	if !strings.Contains(calls[1].Prompt, "region authority") {
		t.Fatal("correction round did not carry the validation error")
	}
}

func TestStatusVocabularyCorrectedInsideAttempt(t *testing.T) {
	db, ctx := setup(t)
	// A status word outside the lifecycle vocabulary must be caught by
	// the proposal validator, where the agent can still correct it —
	// not at commit time, where it would kill the whole attempt.
	bad := `{"prose":"overreach","events":[{"type":"development","title":"The queen's council ends the old alliance","prose":"p","changes":[{"object":"entity","object_id":"q-elinor","op":"set_status","patch":{"status":"deceased"}}]}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: bad},
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	res, err := e.RunStep(ctx)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ModelCalls != 4 {
		t.Fatalf("expected 4 calls with one correction, got %d", res.ModelCalls)
	}
	if !strings.Contains(fake.Calls()[1].Prompt, "set_status needs") {
		t.Error("correction round did not carry the vocabulary error")
	}
}

func TestRetriesExhaustedLeavesStepUncommitted(t *testing.T) {
	db, ctx := setup(t)
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: "not json at all"},
		{Role: "logographer:king-a", Text: "{still broken"},
		{Role: "logographer:king-a", Text: `{"prose":"x","events":"not a list"}`},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	_, err := e.RunStep(ctx)
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("want *Failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "correction retries") {
		t.Fatalf("reason: %v", err)
	}
	meta, _ := db.GetMeta(ctx)
	if meta.ClockStep != 0 || meta.Revision != 0 {
		t.Fatalf("clock moved on failed attempt: %+v", meta)
	}
	attempts, _ := db.ListAttempts(ctx, 5)
	if len(attempts) != 1 || attempts[0].Status != "failed" {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestOrchestratorPauseDoesNotRewriteHistory(t *testing.T) {
	_, ctx := setup(t)
	orch := `{"notes":"impossible","events":[],"pause":{"reason":"hard directive conflicts with canonical history","directive_id":"dir-000001"}}`
	w := testWorld()
	w.Directives = []struct {
		ID   domain.ID `json:"id,omitempty"`
		Text string    `json:"text"`
		Hard bool      `json:"hard,omitempty"`
	}{{ID: "dir-000001", Text: "The king must die this week.", Hard: true}}
	db2, err := store.Open(filepath.Join(t.TempDir(), "w2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.InitWorld(ctx, w); err != nil {
		t.Fatal(err)
	}
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orch},
	}}
	e := &Engine{DB: db2, Client: fake, ModelName: "fake"}
	_, err = e.RunStep(ctx)
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("want *Failure, got %v", err)
	}
	if !strings.Contains(f.Reason, "dir-000001") {
		t.Fatalf("pause reason should name the directive: %q", f.Reason)
	}
	meta, _ := db2.GetMeta(ctx)
	if meta.ClockStep != 0 {
		t.Fatal("clock moved on pause")
	}
	// The pause is durable world state: it blocks later steps until a
	// human resumes. The scripted client is exhausted, so if the engine
	// tried to call the model the error would be about replies instead.
	if reason, paused, err := db2.WorldPause(ctx); err != nil || !paused || !strings.Contains(reason, "dir-000001") {
		t.Fatalf("world not paused durably: %q %v %v", reason, paused, err)
	}
	if _, err := e.RunStep(ctx); !errors.Is(err, ErrPaused) {
		t.Fatalf("second step should be refused with ErrPaused, got %v", err)
	}
	if err := db2.ResumeWorld(ctx); err != nil {
		t.Fatal(err)
	}
	if reason, paused, _ := db2.WorldPause(ctx); paused {
		t.Fatalf("still paused after resume: %q", reason)
	}
}

func TestOrchestratorIdMistakeCorrected(t *testing.T) {
	db, ctx := setup(t)

	bad := `{"notes":"oops","events":[{"origin":"king-a","type":"development","title":"Clash","prose":"At the ford.","changes":[],"affects_entities":["prin-b"],"delivery":{"prin-b":2}}]}`
	good := `{"notes":"fixed","events":[{"origin":"king-a","type":"development","title":"Clash","prose":"At the ford.","changes":[],"affects_entities":["q-elinor"],"delivery":{"prin-b":2}}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: bad},
		{Role: "orchestrator", Text: good},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatalf("step should recover via correction: %v", err)
	}
	calls := fake.Calls()
	if len(calls) != 4 {
		t.Fatalf("want 4 model calls, got %d", len(calls))
	}
	if !strings.Contains(calls[3].Prompt, `unknown entity "prin-b"`) {
		t.Fatal("correction did not name the bad id")
	}
	events, _ := db.ListEvents(ctx, store.EventQuery{})
	if len(events) != 1 {
		t.Fatalf("want 1 committed event, got %d", len(events))
	}
}

func TestTruncatedReplyCorrected(t *testing.T) {
	db, ctx := setup(t)

	orchGood := `{"notes":"resolved","events":[{"origin":"king-a","type":"development","title":"Border clash","prose":"Skirmish at the ford.","changes":[],"delivery":{"prin-b":2}}],"directive_status":[],"reactions":{},"pause":null}`
	// Same reply but flagged as cut by the backend: must be rejected even
	// though it parses, and the correction must say why.
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchGood, Truncated: true},
		{Role: "orchestrator", Text: orchGood},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatalf("step should recover after truncation correction: %v", err)
	}
	calls := fake.Calls()
	if n := len(calls); n != 4 {
		t.Fatalf("want 4 model calls, got %d", n)
	}
	if !strings.Contains(calls[3].Prompt, "cut off at the output limit") {
		t.Fatal("truncation correction missing")
	}
	// The truncated reply must not have been applied a second time.
	events, _ := db.ListEvents(ctx, store.EventQuery{})
	if len(events) != 1 {
		t.Fatalf("event applied twice: %d events", len(events))
	}
}

func TestReactionRound(t *testing.T) {
	db, ctx := setup(t)
	// Step 1 commits a scare authored by prin-b itself, so the event is
	// legitimately visible to B at step 2: a reaction may rest on it.
	bScare := `{"prose":"Watchfires on the march.","events":[{"type":"development","title":"Watchfires on the march","prose":"B's march wards light signal fires.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"mood":"grim"}}]}],"memory":{"summary":"Watchfires lit."}}`
	orchScare := `{"notes":"A local alert.","events":[{"origin":"prin-b","type":"development","title":"Watchfires on the march","prose":"B's march wards light signal fires.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"mood":"grim"}}]}]}`
	prinReact := `{"prose":"The principality mobilizes cautiously.","events":[{"type":"interaction","title":"Border guards on alert","prose":"B's garrisons watch the frontier.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"alert":"elevated"}}]}],"memory":{"summary":"Alert on the border."}}`
	orch1 := `{"notes":"need B's reaction","events":[],"reactions":{"prin-b":{"question":"How does your principality respond to its own watchfire scare?","premise":["evt-000001"]}}}`
	orch2 := `{"notes":"Merged the reaction.","events":[{"origin":"prin-b","type":"interaction","title":"Border guards on alert","prose":"B's garrisons watch the frontier.","causes":["evt-000001"],"changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"alert":"elevated"}}]}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: bScare},
		{Role: "orchestrator", Text: orchScare},
		// step 2
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orch1},
		{Role: "logographer:prin-b", Text: prinReact},
		// Second adjudication round: the reaction is merged into the
		// step; no further reactions are requested, which ends the loop.
		{Role: "orchestrator", Text: orch2},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	for i := 0; i < 2; i++ {
		if _, err := e.RunStep(ctx); err != nil {
			for j, c := range fake.Calls() {
				t.Logf("call %d role=%s", j, c.Role)
			}
			t.Fatalf("step %d: %v", i+1, err)
		}
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Regions["prin-b"].Attrs["alert"] != "elevated" {
		t.Fatalf("reaction not committed: %v", snap.Regions["prin-b"].Attrs)
	}

	// The reaction question and its disclosed premise must have been
	// delivered to B's historian.
	rc := fake.Calls()[6] // prin-b's reaction call
	if !strings.Contains(rc.Prompt, "respond to its own watchfire scare") {
		t.Fatal("orchestrator question missing from reaction prompt")
	}
	if !strings.Contains(rc.Prompt, "events you can see in your own history") {
		t.Fatal("reaction premise disclosure missing from prompt")
	}
}

func TestReactionPremiseCannotLeakDelayedNews(t *testing.T) {
	db, ctx := setup(t)
	// Step 1: the riot is committed; news reaches prin-b only at step 3.
	// Step 2: the orchestrator tries to ask prin-b about it — the
	// premise is not knowable yet, so the correction round must teach
	// that, and no reaction call may happen.
	orchSecretTooEarly := `{"notes":"ask B","events":[],"reactions":{"prin-b":{"question":"How does the principality react to the rioting?","premise":["evt-000001"]}}}`
	orchCorrected := `{"notes":"ask nothing","events":[]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchSecretTooEarly},
		{Role: "orchestrator", Text: orchCorrected},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	for i := 0; i < 2; i++ {
		if _, err := e.RunStep(ctx); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
	}
	if !strings.Contains(fake.Calls()[6].Prompt, "not knowable by that region yet") {
		t.Error("correction round did not carry the visibility teaching")
	}
	// No reaction to the secret ever reached a historian.
	for _, c := range fake.Calls() {
		if strings.Contains(c.Prompt, "ORCHESTRATOR ASKS YOU") {
			t.Fatal("reaction call leaked undelivered news")
		}
	}
	// The step still committed the riot normally.
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Regions["king-a"].Attrs["unrest"] != "high" {
		t.Fatalf("step after correction: %v", snap.Regions["king-a"].Attrs)
	}
}

func TestReactionBudgetExhaustedForcesFinalAdjudication(t *testing.T) {
	db, ctx := setup(t)
	bMarket := `{"prose":"A market decree.","events":[{"type":"development","title":"Border market closed","prose":"The ford market shuts by decree.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"market":"shut"}}]}]}`
	// Step 1 commits an event prin-b can see, so a reaction premise is
	// valid in step 2.
	if _, err := (&Engine{DB: db, Client: &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: bMarket},
		{Role: "orchestrator", Text: `{"notes":"A local decree.","events":[{"origin":"prin-b","type":"development","title":"Border market closed","prose":"The ford market shuts by decree.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"market":"shut"}}]}]}`},
	}}}).RunStep(ctx); err != nil {
		t.Fatal(err)
	}
	// Step 2: the orchestrator asks twice (rounds 0 and 1 within the
	// budget of 2), then at the final adjudication call it still tries
	// to ask — rejected in-turn, and it resolves instead.
	ask := `{"notes":"need an answer","events":[],"reactions":{"prin-b":{"question":"Close the ford too?","premise":["evt-000001"]}}}`
	resolve := `{"notes":"Resolved without asking.","events":[{"origin":"prin-b","type":"interaction","title":"The ford is shut","prose":"Garrisons bar the crossing.","causes":["evt-000001"],"changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"ford":"shut"}}]}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask}, // round 0: within budget
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask}, // round 1: still within budget
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask},     // final call: rejected in-turn
		{Role: "orchestrator", Text: resolve}, // final call: resolves
	}}
	res, err := (&Engine{DB: db, Client: fake, ModelName: "fake"}).RunStep(ctx)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ModelCalls != 8 {
		t.Fatalf("expected 2 initial + 4 orchestrator + 2 reaction calls, got %+v", res)
	}
	if !strings.Contains(fake.Calls()[7].Prompt, "final adjudication call") {
		t.Error("final-call rejection did not teach the orchestrator")
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Regions["prin-b"].Attrs["ford"] != "shut" {
		t.Fatalf("final adjudication not committed: %v", snap.Regions["prin-b"].Attrs)
	}
}

func TestReactionBudgetPersistentAskingFailsAttempt(t *testing.T) {
	db, ctx := setup(t)
	bMarket := `{"prose":"A market decree.","events":[{"type":"development","title":"Border market closed","prose":"The ford market shuts by decree.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"market":"shut"}}]}]}`
	if _, err := (&Engine{DB: db, Client: &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: bMarket},
		{Role: "orchestrator", Text: `{"notes":"A local decree.","events":[{"origin":"prin-b","type":"development","title":"Border market closed","prose":"The ford market shuts by decree.","changes":[{"object":"region","object_id":"prin-b","op":"set_attrs","patch":{"market":"shut"}}]}]}`},
	}}}).RunStep(ctx); err != nil {
		t.Fatal(err)
	}
	ask := `{"notes":"need an answer","events":[],"reactions":{"prin-b":{"question":"Close the ford too?","premise":["evt-000001"]}}}`
	// Exhaust the reaction budget with requests, then keep asking at the
	// final call until correction retries run out.
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask},
		{Role: "orchestrator", Text: ask},
		{Role: "orchestrator", Text: ask},
	}}
	_, err := (&Engine{DB: db, Client: fake, ModelName: "fake"}).RunStep(ctx)
	if err == nil {
		t.Fatal("unresolved reaction request committed as success")
	}
	if !strings.Contains(err.Error(), "final adjudication call") {
		t.Errorf("failure must name the exhausted-reaction cause: %v", err)
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Meta.ClockStep != 1 {
		t.Fatalf("failed attempt advanced the clock: %d", snap.Meta.ClockStep)
	}
	if snap.Regions["prin-b"].Attrs["ford"] != nil {
		t.Fatal("failed attempt changed canonical state")
	}
}

func TestZeroReactionBudgetRejectsAnyRequest(t *testing.T) {
	db, ctx := setup(t)
	b := config.DefaultBudgets()
	b.MaxReactionRounds = 0
	ask := `{"notes":"need an answer","events":[],"reactions":{"prin-b":{"question":"why?","premise":["evt-000001"]}}}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: prinQuiet},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: ask},
		{Role: "orchestrator", Text: ask},
		{Role: "orchestrator", Text: ask},
	}}
	_, err := (&Engine{DB: db, Client: fake, ModelName: "fake", Budgets: b}).RunStep(ctx)
	if err == nil || !strings.Contains(err.Error(), "final adjudication call") {
		t.Fatalf("with zero reaction rounds any request must fail: %v", err)
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Meta.ClockStep != 0 {
		t.Fatalf("failed attempt advanced the clock: %d", snap.Meta.ClockStep)
	}
}

func TestConsultRoundGroundsProposal(t *testing.T) {
	db, ctx := setup(t)
	// One cataloged reference visible to king-a (keywords do not match
	// its context), one private to prin-b.
	if _, err := db.CreateReference(ctx, domain.ReferenceDoc{
		Scope: "region", RegionID: "king-a", Title: "Granary law",
		Body:        "No hoarding in famine years under penalty of the square.",
		Description: "old granary law", Keywords: "hoarding famine",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateReference(ctx, domain.ReferenceDoc{
		Scope: "region", RegionID: "prin-b", Title: "Prince's cipher",
		Body: "SECRET CIPHER TEXT", Description: "private codes", Keywords: "cipher",
	}); err != nil {
		t.Fatal(err)
	}

	kingConsult := `{"prose":"I should check the old granary law before proposing.","events":[],"memory":{"summary":"Consulting the old law."},"consult":["ref-000001","ref-999999","ref-000002"]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingConsult},
		{Role: "logographer:king-a:consult", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	res, err := e.RunStep(ctx)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}

	// The follow-up call happened, and its prompt carried the requested
	// body — but not another region's reference (ref-000002 dropped: not
	// visible; ref-999999 dropped: unknown).
	if res.ModelCalls != 4 {
		t.Fatalf("model calls: %+v", res)
	}
	consultPrompt := fake.Calls()[1].Prompt
	if !strings.Contains(consultPrompt, "REFERENCE DOCUMENTS YOU REQUESTED") ||
		!strings.Contains(consultPrompt, "No hoarding in famine years") {
		t.Errorf("consult prompt lacks requested body")
	}
	if strings.Contains(consultPrompt, "SECRET CIPHER TEXT") {
		t.Error("consult leaked another region's reference")
	}

	// The consulted reply, not the preliminary one, is the proposal.
	props, _ := db.ListProposals(ctx, res.AttemptID)
	if len(props) != 2 {
		t.Fatalf("proposals: %+v", props)
	}
	if props[0].Author != "logographer:king-a" || props[0].Prose != "Unrest spreads." {
		t.Errorf("consulted reply not recorded as proposal: %+v", props[0])
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Regions["king-a"].Attrs["unrest"] != "high" {
		t.Error("consulted proposal not committed")
	}
}

func TestConsultWithoutUsableIDsCostsNoCall(t *testing.T) {
	db, ctx := setup(t)
	kingGuess := `{"prose":"Something about old laws.","events":[],"memory":{"summary":"A fragile monarchy."},"consult":["ref-777777","ref-888888"]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingGuess},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatalf("run step: %v", err)
	}
	if len(fake.Calls()) != 3 {
		t.Errorf("unknown consult ids must not spend a call: %d calls", len(fake.Calls()))
	}
}

func TestVisibilitySeparation(t *testing.T) {
	db, ctx := setup(t)
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: kingReply},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: orchAccept},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatal(err)
	}

	// During the step, neither historian sees the other's canonical conditions.
	calls := fake.Calls()
	kingPrompt, prinPrompt := calls[0].Prompt, calls[1].Prompt
	if !strings.Contains(kingPrompt, "unrest") {
		t.Fatal("king-a prompt missing its own conditions")
	}
	if strings.Contains(prinPrompt, "unrest") {
		t.Fatal("prin-b prompt leaked king-a canonical conditions")
	}
	if !strings.Contains(prinPrompt, "ambition") {
		t.Fatal("prin-b prompt missing its own conditions")
	}
	if strings.Contains(kingPrompt, "ambition") {
		t.Fatal("king-a prompt leaked prin-b canonical conditions")
	}

	// The report becomes visible only at its earliest step.
	snap, _ := db.LoadSnapshot(ctx)
	view, err := knowledge.BuildLogographerView(ctx, snap, "prin-b", db)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Reports) != 0 {
		t.Fatal("report visible before delivery")
	}

	// Run the quiet steps 2 and 3: prin-b's historian must see the news
	// during step 3 (earliest step 3) and not during step 2.
	quiet := `{"prose":"quiet","events":[]}`
	orchNone := `{"notes":"nothing to resolve","events":[]}`
	fake2 := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: quiet},
		{Role: "logographer:prin-b", Text: quiet},
		{Role: "orchestrator", Text: orchNone},
		{Role: "logographer:king-a", Text: quiet},
		{Role: "logographer:prin-b", Text: quiet},
		{Role: "orchestrator", Text: orchNone},
	}}
	e2 := &Engine{DB: db, Client: fake2, ModelName: "fake"}
	for i := 0; i < 2; i++ {
		if _, err := e2.RunStep(ctx); err != nil {
			t.Fatalf("quiet step %d: %v", i+2, err)
		}
	}
	quietCalls := fake2.Calls()
	step2Prompt, step3Prompt := quietCalls[1].Prompt, quietCalls[4].Prompt
	if strings.Contains(step2Prompt, "Rioting in the capital") {
		t.Fatal("news visible to prin-b before its earliest step")
	}
	if !strings.Contains(step3Prompt, "Rioting in the capital") {
		t.Fatal("news not delivered to prin-b at its earliest step")
	}
	if !strings.Contains(step3Prompt, "DELIVERED REPORTS") {
		t.Fatal("delivery section missing from prin-b step 3 prompt")
	}

	if _, err := db.BeginAttempt(ctx, 4); err != nil {
		t.Fatal(err)
	}
	snap, _ = db.LoadSnapshot(ctx)
	view, err = knowledge.BuildLogographerView(ctx, snap, "prin-b", db)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Reports) != 1 || view.Reports[0].Title != "Rioting in the capital" {
		t.Fatalf("report not delivered: %+v", view.Reports)
	}
}

func TestBeliefsRecordedAndScopedToRegion(t *testing.T) {
	db, ctx := setup(t)
	king := `{"prose":"Rumors spread.","events":[],"beliefs":[{"subject_id":"q-elinor","modality":"mistaken","statement":"The queen has already fled the capital."},{"modality":"uncertain","statement":"Grain ships will arrive within the fortnight."}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: king},
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: `{"notes":"Quiet step.","events":[]}`},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := db.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Beliefs["king-a"]) != 2 || len(snap.Beliefs["prin-b"]) != 0 {
		t.Fatalf("belief scoping wrong: %v", snap.Beliefs)
	}
	bl := snap.Beliefs["king-a"][0]
	if bl.Modality != "mistaken" || bl.SubjectID != "q-elinor" || bl.AsOfStep != 1 {
		t.Errorf("belief stored wrong: %+v", bl)
	}
}

func TestBeliefBadInputFeedsCorrection(t *testing.T) {
	db, ctx := setup(t)
	bad := `{"prose":"x","events":[],"beliefs":[{"subject_id":"ghost-999","modality":"certain","statement":"nonsense"}]}`
	good := `{"prose":"fixed","events":[],"beliefs":[{"modality":"uncertain","statement":"The harvest will be short."}]}`
	fake := &inference.Scripted{Replies: []inference.Reply{
		{Role: "logographer:king-a", Text: bad},
		{Role: "logographer:king-a", Text: good}, // correction round
		{Role: "logographer:prin-b", Text: prinQuiet},
		{Role: "orchestrator", Text: `{"notes":"Quiet step.","events":[]}`},
	}}
	e := &Engine{DB: db, Client: fake, ModelName: "fake"}
	if _, err := e.RunStep(ctx); err != nil {
		t.Fatalf("corrected reply should commit: %v", err)
	}
	snap, _ := db.LoadSnapshot(ctx)
	if len(snap.Beliefs["king-a"]) != 1 || snap.Beliefs["king-a"][0].SubjectID != "" {
		t.Fatalf("only the corrected belief should exist: %+v", snap.Beliefs["king-a"])
	}
}
