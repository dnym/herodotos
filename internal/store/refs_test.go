package store

import (
	"context"
	"strings"
	"testing"

	"github.com/dnym/herodotos/internal/domain"
)

func TestReferenceCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateReference(ctx, domain.ReferenceDoc{
		Scope: "world", Title: "Magic rules", Body: "Magic is local and costly.",
		Description: "How magic works and what it costs", Keywords: "magic ritual",
		Binding: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "ref-") {
		t.Errorf("id = %q", id)
	}

	// Region scope validation.
	if _, err := db.CreateReference(ctx, domain.ReferenceDoc{
		Scope: "region", Title: "no region", Body: "x",
	}); err == nil {
		t.Error("region-scoped ref without region id must fail")
	}
	if _, err := db.CreateReference(ctx, domain.ReferenceDoc{
		Scope: "world", Title: "", Body: "x",
	}); err == nil {
		t.Error("ref without title must fail")
	}

	// Snapshot must see it (enabled by default).
	snap, err := db.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.WorldRefs) != 1 || !snap.WorldRefs[0].Binding ||
		snap.WorldRefs[0].Description == "" || !snap.WorldRefs[0].Enabled {
		t.Errorf("snapshot refs wrong: %+v", snap.WorldRefs)
	}

	// Update, including disabling.
	ref := snap.WorldRefs[0]
	ref.Enabled = false
	ref.Body = "Magic is local, costly, and fading."
	ok, err := db.UpdateReference(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("update ok=%v err=%v", ok, err)
	}
	snap, err = db.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.WorldRefs) != 0 {
		t.Errorf("disabled reference still visible: %+v", snap.WorldRefs)
	}

	if ok, err := db.DeleteReference(ctx, ref.ID); err != nil || !ok {
		t.Fatalf("delete ok=%v err=%v", ok, err)
	}
	if ok, _ := db.UpdateReference(ctx, ref); ok {
		t.Error("update of deleted reference reported success")
	}
}

func TestParseRefMarkdown(t *testing.T) {
	doc := `---
title: How sea magic works
description: Tides, binding costs, and why mages cannot cross the strait
scope: region
region: prin-b
binding: true
keywords: sea magic tides
---

# Sea magic

Salt, blood, and distance: a mage binds only what they have touched.
`
	ref, err := ParseRefMarkdown(doc)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Title != "How sea magic works" || ref.Scope != "region" || ref.RegionID != "prin-b" ||
		!ref.Binding || ref.Keywords != "sea magic tides" || ref.Description == "" {
		t.Errorf("front matter parsed wrong: %+v", ref)
	}
	if !strings.Contains(ref.Body, "binds only what they have touched") {
		t.Errorf("body wrong: %q", ref.Body)
	}

	// No front matter: title comes from the first heading, scope defaults to world.
	plain, err := ParseRefMarkdown("# Grain law\n\nNo hoarding in famine years.")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Title != "Grain law" || plain.Scope != "world" {
		t.Errorf("plain parse: %+v", plain)
	}

	if _, err := ParseRefMarkdown("---\nbogus: 1\n---\nbody"); err == nil {
		t.Error("unknown front matter key must fail")
	}
	if _, err := ParseRefMarkdown("---\ntitle: unclosed\nbody"); err == nil {
		t.Error("unclosed front matter must fail")
	}
	if _, err := ParseRefMarkdown(""); err == nil {
		t.Error("empty document must fail validation")
	}
}

// Explicit ids inside the generated namespace must not wedge automatic
// allocation: the allocator skips taken ids instead of colliding on
// every retry.
func TestAutomaticIDsSkipExplicitOnes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, id := range []domain.ID{"ref-000001", "ref-000002"} {
		if _, err := db.CreateReference(ctx, domain.ReferenceDoc{
			ID: id, Scope: "world", Title: "custom " + id, Body: "x",
		}); err != nil {
			t.Fatalf("explicit %s: %v", id, err)
		}
	}
	for i := 0; i < 2; i++ {
		id, err := db.CreateReference(ctx, domain.ReferenceDoc{Scope: "world", Title: "auto", Body: "y"})
		if err != nil {
			t.Fatalf("automatic allocation %d: %v", i, err)
		}
		if id == "ref-000001" || id == "ref-000002" {
			t.Fatalf("allocator reused an explicit id: %s", id)
		}
	}
	refs, err := db.ListReferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 4 {
		t.Fatalf("want 4 references, got %d", len(refs))
	}

	// Same for directives created through the editor after an import
	// claimed generated-looking ids.
	if _, err := db.sqlDB.ExecContext(ctx,
		`INSERT INTO directives(id, text, hard, status) VALUES ('dir-000001', 'imported', 0, 'pending')`); err != nil {
		t.Fatal(err)
	}
	if id, err := db.CreateDirective(ctx, "new", false); err != nil || id == "dir-000001" {
		t.Fatalf("directive allocation: id=%q err=%v", id, err)
	}
}
