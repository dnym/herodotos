package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dnym/herodotos/internal/domain"
)

func commitSimple(t *testing.T, d *DB, title string, beliefs []domain.Belief, reports []ReportPlan) {
	t.Helper()
	ctx := context.Background()
	meta, err := d.GetMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.CommitStep(ctx, CommitPlan{
		BaseRevision: meta.Revision,
		Summary:      "test",
		Events: []CommittedEvent{{
			Type: "development", Title: title, Prose: "Something happens.",
		}},
		Beliefs: beliefs,
		Reports: reports,
	})
	if err != nil {
		t.Fatalf("commit %q: %v", title, err)
	}
}

func TestBeliefsCommitAndPrune(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	var many []domain.Belief
	for i := 0; i < 14; i++ {
		many = append(many, domain.Belief{
			RegionID: "king-a", SubjectID: "q-elinor",
			Modality: "uncertain", Statement: "belief " + string(rune('a'+i)),
		})
	}
	commitSimple(t, d, "first", many, nil)

	snap, err := d.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Beliefs["king-a"]) != maxBeliefsPerRegion {
		t.Fatalf("pruned to %d beliefs, want %d", len(snap.Beliefs["king-a"]), maxBeliefsPerRegion)
	}
	last := snap.Beliefs["king-a"][len(snap.Beliefs["king-a"])-1]
	if last.Statement != "belief n" || last.SubjectID != "q-elinor" || last.AsOfStep != 1 {
		t.Errorf("kept belief = %+v, want newest inserted with subject and step", last)
	}
	// The two oldest must be gone.
	for _, bl := range snap.Beliefs["king-a"] {
		if bl.Statement == "belief a" || bl.Statement == "belief b" {
			t.Errorf("oldest belief %q survived pruning", bl.Statement)
		}
	}
}

func TestDurableWorldPause(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if _, paused, err := d.WorldPause(ctx); err != nil || paused {
		t.Fatalf("fresh world paused=%v err=%v", paused, err)
	}
	attemptID, err := d.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PauseWorld(ctx, attemptID, "hard directive dir-000001 cannot be met"); err != nil {
		t.Fatal(err)
	}
	reason, paused, err := d.WorldPause(ctx)
	if err != nil || !paused || reason != "hard directive dir-000001 cannot be met" {
		t.Fatalf("WorldPause = %q, %v, %v", reason, paused, err)
	}

	// The pause survives a reopen (durable world state, not process state).
	path := d.Path
	d.Close()
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if reason, paused, _ = d2.WorldPause(ctx); !paused || reason == "" {
		t.Fatalf("pause lost across reopen: %q %v", reason, paused)
	}

	if err := d2.ResumeWorld(ctx); err != nil {
		t.Fatal(err)
	}
	if _, paused, _ = d2.WorldPause(ctx); paused {
		t.Fatal("still paused after resume")
	}
}

func TestDirectiveMutationByHuman(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	id, err := d.CreateDirective(ctx, "The eastern coast must change hands by step 8.", true)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := d.ListDirectives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *domain.Directive
	for i := range dirs {
		if dirs[i].ID == id {
			found = &dirs[i]
		}
	}
	if found == nil || !found.Hard || found.Status != "active" {
		t.Fatalf("created directive = %+v", found)
	}

	ok, err := d.UpdateDirectiveText(ctx, id, "Revised: the eastern coast shifts gradually.")
	if err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	dirs, _ = d.ListDirectives(ctx)
	for _, dd := range dirs {
		if dd.ID == id && dd.Text != "Revised: the eastern coast shifts gradually." {
			t.Fatalf("text not updated: %q", dd.Text)
		}
	}
	ok, err = d.UpdateDirectiveText(ctx, "dir-999999", "nope")
	if err != nil || ok {
		t.Fatalf("unknown directive: ok=%v err=%v", ok, err)
	}
}

func TestCreativeInputEditsInvalidateInFlightSteps(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Creative input changes bump the revision, so a step that
	// snapshotted the old rules fails its commit check.
	id, err := d.CreateDirective(ctx, "A hard rule.", true)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := d.GetMeta(ctx)
	if meta.Revision != 1 {
		t.Fatalf("directive add must bump revision: %d", meta.Revision)
	}
	att, _ := d.BeginAttempt(ctx, 1)
	if _, err := d.UpdateDirectiveText(ctx, id, "Reworded rule."); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitStep(ctx, CommitPlan{
		AttemptID: att, BaseRevision: meta.Revision, Summary: "stale",
		Events: []CommittedEvent{{Type: "t", Title: "x", Prose: "x"}},
	}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale commit after reword: %v", err)
	}

	// Rewording resets the assessment status: an old verdict does not
	// describe new text.
	att, _ = d.BeginAttempt(ctx, 1)
	if _, err := d.CommitStep(ctx, CommitPlan{
		AttemptID: att, BaseRevision: 2, Summary: "verdict",
		Events: []CommittedEvent{{Type: "t", Title: "x", Prose: "x",
			Changes: []domain.Change{{Object: "directive", ObjectID: domain.ID(id), Op: "set_status", Patch: map[string]any{"status": "fulfilled"}}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateDirectiveText(ctx, id, "Even newer text."); err != nil {
		t.Fatal(err)
	}
	dirs, _ := d.ListDirectives(ctx)
	for _, dd := range dirs {
		if dd.ID == id && dd.Status != "pending" {
			t.Fatalf("reword kept stale status: %+v", dd)
		}
	}

	// Reference edits bump too; failed no-op edits do not.
	revBefore, _ := d.GetMeta(ctx)
	if _, err := d.CreateReference(ctx, domain.ReferenceDoc{
		Scope: "world", Title: "Rules", Body: "No magic.", Binding: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if meta, _ := d.GetMeta(ctx); meta.Revision != revBefore.Revision+1 {
		t.Fatalf("reference add must bump revision: %d", meta.Revision)
	}
	if ok, err := d.UpdateReference(ctx, domain.ReferenceDoc{
		ID: "ref-999999", Scope: "world", Title: "x", Body: "y", Enabled: true,
	}); err != nil || ok {
		t.Fatalf("unknown reference update: ok=%v err=%v", ok, err)
	}
	if meta, _ := d.GetMeta(ctx); meta.Revision != revBefore.Revision+1 {
		t.Fatalf("no-op edit bumped revision: %d", meta.Revision)
	}
}

func TestExportBundle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	meta, _ := d.GetMeta(ctx)
	if _, err := d.CommitStep(ctx, CommitPlan{
		BaseRevision: meta.Revision,
		Events: []CommittedEvent{{
			Type: "conflict", Title: "Border skirmish", Prose: "Militias clash.",
			RegionIDs: []domain.ID{"king-a", "prin-b"},
		}},
		Beliefs: []domain.Belief{{RegionID: "prin-b", Modality: "mistaken", Statement: "king-a is bankrupt."}},
		Reports: []ReportPlan{{EventIndex: 0, TargetRegion: "prin-b", EarliestStep: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitStep(ctx, CommitPlan{
		BaseRevision: meta.Revision + 1,
		Events: []CommittedEvent{{
			Type: "development", Title: "Second event", Prose: "Aftermath.",
			CauseIDs: []domain.ID{"evt-000001"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	b, err := d.ExportBundle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.Meta.ClockStep != 2 || len(b.Events) != 2 || len(b.Regions) != 2 {
		t.Fatalf("export shape: step %d, %d events, %d regions", b.Meta.ClockStep, len(b.Events), len(b.Regions))
	}
	if b.Events[0].Title != "Border skirmish" || b.Events[1].Title != "Second event" {
		t.Error("events not in chronological order")
	}
	if len(b.Reports) != 1 || b.Reports[0].DeliveredStep != 0 {
		t.Errorf("reports wrong: %+v", b.Reports)
	}
	if len(b.Beliefs) != 1 || b.Beliefs[0].Modality != "mistaken" {
		t.Errorf("beliefs wrong: %+v", b.Beliefs)
	}
	if len(b.Connections) != 1 || len(b.Memories) != 2 {
		t.Errorf("missing geography/memory in export")
	}
	if b.Pause != nil {
		t.Errorf("paused flag in an unpaused export: %+v", b.Pause)
	}
	if err := d.PauseWorld(ctx, "", "hard directive unresolvable"); err != nil {
		t.Fatal(err)
	}
	if b, err = d.ExportBundle(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Pause == nil || b.Pause.Reason != "hard directive unresolvable" {
		t.Errorf("durable pause missing from export: %+v", b.Pause)
	}
	// Must round-trip as JSON with snake_case keys.
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	ev0 := generic["events"].([]any)[0].(map[string]any)
	if ev0["cause_ids"] == nil || ev0["region_ids"] == nil {
		t.Errorf("event json keys: %v", ev0)
	}
}
