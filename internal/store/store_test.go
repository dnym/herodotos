package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dnym/herodotos/internal/domain"
)

func testWorld() *WorldSpec {
	w := &WorldSpec{
		Name:      "Test World",
		StartDate: "1000-06-01",
		StepDays:  7,
	}
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

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "world.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.InitWorld(context.Background(), testWorld()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return d
}

func TestHappyCommitAndReopen(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	att, err := d.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	if err := d.RecordProposal(ctx, att, domain.Proposal{
		ID: "prop-000001", Author: "logographer:king-a", Kind: "development", Prose: "unrest grows",
	}); err != nil {
		t.Fatalf("record proposal: %v", err)
	}

	ids, err := d.CommitStep(ctx, CommitPlan{
		AttemptID:    att,
		BaseRevision: 0,
		Summary:      "week 1",
		Events: []CommittedEvent{{
			Type: "development", Title: "Rioting in the capital", Prose: "Crowds gather.",
			RegionIDs: []domain.ID{"king-a"}, EntityIDs: []domain.ID{"q-elinor"},
			Changes: []domain.Change{{
				Object: "region", ObjectID: "king-a", Op: "set_attrs",
				Patch: map[string]any{"unrest": "high"},
			}},
		}},
		Reports: []ReportPlan{{EventIndex: 0, TargetRegion: "prin-b", EarliestStep: 3}},
		Memories: []domain.Memory{{
			RegionID: "king-a", Summary: "Rioting began; the queen is uneasy.", UpdatedStep: 1,
		}},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(ids) != 1 || ids[0] != "evt-000001" {
		t.Fatalf("event ids = %v", ids)
	}

	meta, err := d.GetMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ClockStep != 1 || meta.Revision != 1 {
		t.Fatalf("clock=%d revision=%d", meta.ClockStep, meta.Revision)
	}

	// Reopen: everything must come back unchanged.
	d.Close()
	d2, err := Open(d.Path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	snap, err := d2.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := snap.Regions["king-a"].Attrs["unrest"]; got != "high" {
		t.Fatalf("region attrs after commit: %v", snap.Regions["king-a"].Attrs)
	}
	if len(snap.RecentEvents) != 1 || snap.RecentEvents[0].ID != "evt-000001" {
		t.Fatalf("recent events: %v", snap.RecentEvents)
	}
	if len(snap.DeliveredReports["prin-b"]) != 0 {
		t.Fatalf("report delivered too early: %v", snap.DeliveredReports["prin-b"])
	}
	if snap.Memories["king-a"].Summary == "" {
		t.Fatal("memory not persisted")
	}

	// Report becomes available at its earliest step.
	if _, err := d2.BeginAttempt(ctx, 3); err != nil {
		t.Fatal(err)
	}
	snap, err = d2.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.DeliveredReports["prin-b"]) != 1 {
		t.Fatalf("report not delivered at step 3: %v", snap.DeliveredReports)
	}
}

func TestStaleRevisionRejectsCommit(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	att, _ := d.BeginAttempt(ctx, 1)
	if _, err := d.CommitStep(ctx, CommitPlan{AttemptID: att, BaseRevision: 0, Summary: "one",
		Events: []CommittedEvent{{Type: "t", Title: "one", Prose: "x"}}}); err != nil {
		t.Fatal(err)
	}

	att2, _ := d.BeginAttempt(ctx, 2)
	_, err := d.CommitStep(ctx, CommitPlan{AttemptID: att2, BaseRevision: 0, Summary: "stale",
		Events: []CommittedEvent{{Type: "t", Title: "stale", Prose: "x"}}})
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("want ErrStaleRevision, got %v", err)
	}
	meta, _ := d.GetMeta(ctx)
	if meta.ClockStep != 1 {
		t.Fatalf("clock advanced on rejected commit: %d", meta.ClockStep)
	}
	att3, _ := d.BeginAttempt(ctx, 2)
	if _, err := d.CommitStep(ctx, CommitPlan{AttemptID: att3, BaseRevision: 1, Summary: "two",
		Events: []CommittedEvent{{Type: "t", Title: "two", Prose: "x"}}}); err != nil {
		t.Fatalf("commit after rejection should work: %v", err)
	}
}

func TestInvalidChangeRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	att, _ := d.BeginAttempt(ctx, 1)
	_, err := d.CommitStep(ctx, CommitPlan{
		AttemptID: att, BaseRevision: 0, Summary: "bad",
		Events: []CommittedEvent{{
			Type: "t", Title: "good event", Prose: "fine",
			RegionIDs: []domain.ID{"king-a"},
			Changes: []domain.Change{
				{Object: "region", ObjectID: "king-a", Op: "set_attrs", Patch: map[string]any{"unrest": "high"}},
				{Object: "entity", ObjectID: "no-such-entity", Op: "set_attrs", Patch: map[string]any{"x": 1}},
			},
		}},
	})
	if err == nil {
		t.Fatal("expected failure on unknown entity")
	}
	meta, _ := d.GetMeta(ctx)
	if meta.ClockStep != 0 || meta.Revision != 0 {
		t.Fatalf("failed commit mutated clock: %+v", meta)
	}
	snap, _ := d.LoadSnapshot(ctx)
	if snap.Regions["king-a"].Attrs["unrest"] != "low" {
		t.Fatalf("partial state applied: %v", snap.Regions["king-a"].Attrs)
	}
	if len(snap.RecentEvents) != 0 {
		t.Fatal("partial events applied")
	}
	if err := d.MarkAttemptFailed(ctx, att, err.Error()); err != nil {
		t.Fatal(err)
	}
	attempts, _ := d.ListAttempts(ctx, 5)
	if len(attempts) != 1 || attempts[0].Status != "failed" {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestWriterLock(t *testing.T) {
	d := openTestDB(t)
	if _, err := Open(d.Path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second writer: want ErrLocked, got %v", err)
	}
}

func TestPatchSemantics(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	att, _ := d.BeginAttempt(ctx, 1)
	_, err := d.CommitStep(ctx, CommitPlan{
		AttemptID: att, BaseRevision: 0, Summary: "patch",
		Events: []CommittedEvent{{
			Type: "t", Title: "merge", Prose: "x",
			Changes: []domain.Change{
				{Object: "region", ObjectID: "king-a", Op: "set_attrs", Patch: map[string]any{"unrest": nil, "refugees": 12}},
				{Object: "entity", ObjectID: "rev-movement", Op: "upsert",
					Patch: map[string]any{"kind": "group", "name": "Revolutionary Movement", "region_id": "king-a", "goal": "crown"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := d.LoadSnapshot(ctx)
	r := snap.Regions["king-a"]
	if _, ok := r.Attrs["unrest"]; ok {
		t.Fatal("null did not remove key")
	}
	if r.Attrs["refugees"] != float64(12) {
		t.Fatalf("refugees = %v", r.Attrs["refugees"])
	}
	e, ok := snap.Entities["rev-movement"]
	if !ok || e.Name != "Revolutionary Movement" || e.Attrs["goal"] != "crown" {
		t.Fatalf("entity upsert failed: %+v ok=%v", e, ok)
	}
}
