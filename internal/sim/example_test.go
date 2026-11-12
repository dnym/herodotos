package sim

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dnym/herodotos/internal/inference"
	"github.com/dnym/herodotos/internal/store"
)

// TestFourRegionsExample walks the shipped example world through one
// scripted step: the world file, the fixture, and the engine must stay
// consistent with each other.
func TestFourRegionsExample(t *testing.T) {
	ctx := context.Background()
	spec, err := store.LoadWorldSpec(filepath.Join("..", "..", "examples", "four-regions", "world.json"))
	if err != nil {
		t.Fatalf("example world: %v", err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "four.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitWorld(ctx, spec); err != nil {
		t.Fatalf("init: %v", err)
	}
	fake, err := inference.NewScriptedFromFile(
		filepath.Join("..", "..", "testdata", "model-responses", "four-regions-step1.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	e := &Engine{DB: db, Client: fake, ModelName: "scripted"}
	res, err := e.RunStep(ctx)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if res.Step != 1 || len(res.EventIDs) == 0 {
		t.Fatalf("result: %+v", res)
	}
	snap, _ := db.LoadSnapshot(ctx)
	if snap.Regions["kingdom-a"].Attrs["unrest"] != "simmering" {
		t.Fatalf("Aelmark state: %v", snap.Regions["kingdom-a"].Attrs)
	}
	if snap.Directives["dir-000001"].Status != "active" {
		t.Fatalf("directive not activated: %+v", snap.Directives)
	}
	reports, _ := db.ListReports(ctx)
	if len(reports) == 0 {
		t.Fatal("expected delayed cross-region reports")
	}
	for _, rp := range reports {
		if rp.Report.DeliveredStep != 0 {
			t.Fatalf("news delivered before its earliest step: %+v", rp.Report)
		}
	}
	for _, rid := range []string{"principality-b", "republic-c", "frontier-d"} {
		if len(snap.DeliveredReports[rid]) != 0 {
			t.Fatalf("%s already knows foreign events", rid)
		}
	}
}
