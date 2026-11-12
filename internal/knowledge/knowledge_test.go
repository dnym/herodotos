package knowledge

import (
	"testing"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/store"
)

func TestClassifyReferences(t *testing.T) {
	region := &domain.Region{ID: "king-a", Name: "Kingdom A",
		Attrs: map[string]any{"grain": "full"}}
	v := &LogographerView{Region: region,
		Memory:       &domain.Memory{Summary: "Succession crisis brewing in the granary square."},
		RecentEvents: []domain.Event{{Title: "Grain tax riot"}}}
	s := &store.Snapshot{
		WorldRefs: []domain.ReferenceDoc{
			{ID: "ref-rules", Scope: "world", Binding: true, Title: "World rules", Body: "no gunpowder"},
			{ID: "ref-navy", Scope: "world", Title: "Naval warfare", Body: "galleys and harbors",
				Keywords: "ships harbor navy"},
		},
		RegionRefs: map[domain.ID][]domain.ReferenceDoc{"king-a": {
			// Keywords match memory summary and event titles.
			{ID: "ref-chron", Scope: "region", RegionID: "king-a", Title: "Chronicle",
				Body: "succession by acclamation", Keywords: "succession grain"},
			// Two keyword hits: ranks above the one-hit doc.
			{ID: "ref-famine", Scope: "region", RegionID: "king-a", Title: "Famine book",
				Body: "the price of hoarding", Keywords: "grain famine hoarding"},
			{ID: "ref-trade", Scope: "region", RegionID: "king-a", Title: "Trade routes",
				Body: "salt and amber", Keywords: "grain silver"},
			// No keyword hit: catalog only.
			{ID: "ref-stars", Scope: "region", RegionID: "king-a", Title: "Astronomy",
				Body: "comets and omens", Keywords: "comet stars", Description: "how omens are read"},
		}},
	}

	classifyReferences(v, s, "king-a")

	if len(v.Bindings) != 1 || v.Bindings[0].ID != "ref-rules" {
		t.Errorf("bindings: %+v", v.Bindings)
	}
	var got []string
	for _, r := range v.Retrieved {
		got = append(got, string(r.ID))
	}
	// ref-chron scores 2 (succession, grain); ref-famine and ref-trade
	// score 1 each (grain). Cap is 3, best first, ties by id.
	want := []string{"ref-chron", "ref-famine", "ref-trade"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("retrieved order: got %v", got)
	}
	for i := 1; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			// ties are allowed to differ only by id order; both are stable
			t.Errorf("retrieved[%d]: got %s want %s", i, got[i], want[i])
		}
	}
	if len(v.Catalog) != 2 {
		t.Fatalf("catalog: %+v", v.Catalog)
	}
	byID := map[string]bool{}
	for _, r := range v.Catalog {
		byID[string(r.ID)] = true
	}
	if !byID["ref-stars"] || !byID["ref-navy"] {
		t.Errorf("catalog should hold the unmatched docs: %+v", v.Catalog)
	}
}

func TestClassifyReferencesCap(t *testing.T) {
	region := &domain.Region{ID: "king-a", Name: "A"}
	v := &LogographerView{Region: region}
	s := &store.Snapshot{RegionRefs: map[domain.ID][]domain.ReferenceDoc{"king-a": {}}}
	for _, id := range []string{"ref-d", "ref-c", "ref-b", "ref-a"} {
		s.RegionRefs["king-a"] = append(s.RegionRefs["king-a"], domain.ReferenceDoc{
			ID: id, Scope: "region", RegionID: "king-a", Title: "T", Body: "B", Keywords: "king"})
	}
	classifyReferences(v, s, "king-a")
	if len(v.Retrieved) != maxRetrieved {
		t.Fatalf("retrieved %d, want cap %d", len(v.Retrieved), maxRetrieved)
	}
	// All score equal, so id order decides.
	if v.Retrieved[0].ID != "ref-a" || v.Retrieved[2].ID != "ref-c" {
		t.Errorf("unstable order: %+v", v.Retrieved)
	}
	if len(v.Catalog) != 1 || v.Catalog[0].ID != "ref-d" {
		t.Errorf("overflow should fall back to catalog: %+v", v.Catalog)
	}
}
