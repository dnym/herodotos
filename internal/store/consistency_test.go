package store

import (
	"context"
	"testing"
	"time"

	"github.com/dnym/herodotos/internal/domain"
)

func quietCommit(t *testing.T, d *DB, ctx context.Context) {
	t.Helper()
	meta, err := d.GetMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	att, err := d.BeginAttempt(ctx, meta.ClockStep+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CommitStep(ctx, CommitPlan{
		AttemptID: att, BaseRevision: meta.Revision, Summary: "quiet",
		Events: []CommittedEvent{{
			Type: "development", Title: "Note", Prose: "A quiet week passes.",
			RegionIDs: []domain.ID{"king-a"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExportInternallyConsistent(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	b, err := d.ExportBundle(ctx)
	if err != nil {
		t.Fatalf("empty export: %v", err)
	}
	if b.Meta.ClockStep != 0 || len(b.Events) != 0 {
		t.Fatalf("empty export mixed states: clock=%d events=%d", b.Meta.ClockStep, len(b.Events))
	}
	for i := 0; i < 3; i++ {
		quietCommit(t, d, ctx)
	}
	b, err = d.ExportBundle(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if b.Meta.ClockStep != 3 || len(b.Events) != 3 {
		t.Fatalf("export mismatch: clock=%d events=%d", b.Meta.ClockStep, len(b.Events))
	}
	for _, ev := range b.Events {
		if ev.Step > b.Meta.ClockStep {
			t.Fatalf("export mixes clock %d with event at step %d", b.Meta.ClockStep, ev.Step)
		}
	}
}

func TestConcurrentReadsNeverMixSteps(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			quietCommit(t, d, ctx)
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 60; i++ {
		snap, err := d.LoadSnapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for _, ev := range snap.RecentEvents {
			if ev.Step > snap.Meta.ClockStep {
				t.Fatalf("snapshot mixes clock %d with event at step %d", snap.Meta.ClockStep, ev.Step)
			}
		}
		b, err := d.ExportBundle(ctx)
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		for _, ev := range b.Events {
			if ev.Step > b.Meta.ClockStep {
				t.Fatalf("export mixes clock %d with event at step %d", b.Meta.ClockStep, ev.Step)
			}
		}
	}
	<-done
	if meta, err := d.GetMeta(ctx); err != nil || meta.ClockStep != 20 {
		t.Fatalf("writer did not finish: %+v %v", meta, err)
	}
}
