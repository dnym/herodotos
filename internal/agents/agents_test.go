package agents

import (
	"strings"
	"testing"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/knowledge"
	"github.com/dnym/herodotos/internal/store"
)

func TestParseLogographerReplyFenced(t *testing.T) {
	text := "```json\n{\"prose\":\"quiet\",\"events\":[]}\n```"
	r, err := ParseLogographerReply(text)
	if err != nil {
		t.Fatalf("fenced reply: %v", err)
	}
	if r.Prose != "quiet" || len(r.Events) != 0 {
		t.Fatalf("parsed: %+v", r)
	}
	if _, err := ParseLogographerReply(`{"prose":"x","events":[],"junk":1}`); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestParseRejectsOversizeAndIncomplete(t *testing.T) {
	many := `{"prose":"x","events":[` + strings.Repeat(`{"type":"t","title":"a","prose":"b"},`, maxEventsPerAgent) + `]}`
	if _, err := ParseLogographerReply(many); err == nil {
		t.Fatal("expected event-count rejection")
	}
	if _, err := ParseLogographerReply(`{"prose":"x","events":[{"type":"t"}]}`); err == nil {
		t.Fatal("expected incomplete event rejection")
	}
}

func TestValidateLogographerProposal(t *testing.T) {
	snap := &store.Snapshot{
		Regions:  map[domain.ID]*domain.Region{"king-a": {ID: "king-a"}, "prin-b": {ID: "prin-b"}},
		Entities: map[domain.ID]*domain.Entity{"q-elinor": {ID: "q-elinor", RegionID: "king-a"}},
	}
	pv := &ProposalValidator{Snap: snap, CurrentStep: 4}

	ok := &domain.PropEvent{
		Type: "development", Title: "t", Prose: "p",
		Changes:         []domain.Change{{Object: "entity", ObjectID: "rev-x", Op: "upsert", Patch: map[string]any{"kind": "group", "name": "X", "region_id": "king-a"}}},
		AffectsEntities: []domain.ID{"q-elinor"},
		Delivery:        map[string]int{"prin-b": 6},
	}
	if err := pv.ValidateLogographerProposal("king-a", ok); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}

	badRegion := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "region", ObjectID: "prin-b", Op: "set_attrs", Patch: map[string]any{}}}}
	if err := pv.ValidateLogographerProposal("king-a", badRegion); err == nil {
		t.Fatal("cross-region write accepted")
	}
	badEntity := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "entity", ObjectID: "q-elinor", Op: "set_attrs", Patch: map[string]any{}}}}
	if err := pv.ValidateLogographerProposal("prin-b", badEntity); err == nil {
		t.Fatal("foreign entity write accepted")
	}

	dirEv := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "directive", ObjectID: "dir-000001", Op: "set_status", Patch: map[string]any{"status": "fulfilled"}}}}
	if err := pv.ValidateLogographerProposal("king-a", dirEv); err == nil {
		t.Fatal("directive change accepted")
	}
	tooSoon := &domain.PropEvent{Title: "t", Prose: "p", Delivery: map[string]int{"prin-b": 5}}
	if err := pv.ValidateLogographerProposal("king-a", tooSoon); err == nil {
		t.Fatal("delivery at current+0 boundary accepted")
	}

	// set_status vocabulary must be caught while the agent can still
	// correct it, and the error must teach the vocabulary.
	snap.Relationships = map[domain.ID]*domain.Relationship{"rel-1": {ID: "rel-1", Status: "active"}}
	relBad := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "relationship", ObjectID: "rel-1", Op: "set_status", Patch: map[string]any{"status": "under negotiation"}}}}
	err := pv.ValidateLogographerProposal("king-a", relBad)
	if err == nil {
		t.Fatal("out-of-vocabulary relationship status accepted at proposal time")
	}
	if !strings.Contains(err.Error(), "active or ended") {
		t.Errorf("rejection must teach the vocabulary: %v", err)
	}
	if err := pv.ValidateFinalEvent(relBad); err == nil {
		t.Fatal("out-of-vocabulary status accepted in orchestrator output")
	}
	relGood := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "relationship", ObjectID: "rel-1", Op: "set_status", Patch: map[string]any{"status": "ended"}}}}
	if err := pv.ValidateLogographerProposal("king-a", relGood); err != nil {
		t.Errorf("valid status rejected: %v", err)
	}
	if err := pv.ValidateFinalEvent(relGood); err != nil {
		t.Errorf("valid status rejected in final event: %v", err)
	}
}

func TestDetectInteractions(t *testing.T) {
	proposals := map[string][]domain.PropEvent{
		"logographer:king-a": {{
			Changes: []domain.Change{
				{Object: "region", ObjectID: "king-a", Op: "set_attrs"},
				{Object: "entity", ObjectID: "army-b", Op: "upsert", Patch: map[string]any{"region_id": "prin-b"}},
			},
			AffectsEntities: []domain.ID{"q-elinor"},
		}},
		"logographer:prin-b": {{
			Changes:         []domain.Change{{Object: "entity", ObjectID: "army-b", Op: "upsert", Patch: map[string]any{"region_id": "prin-b"}}},
			AffectsEntities: []domain.ID{"q-elinor"},
		}},
	}
	got := DetectInteractions(proposals)
	kinds := map[string]bool{}
	for _, in := range got {
		kinds[in.Kind] = true
	}
	// army-b is a shared write target, q-elinor a shared participant,
	// and king-a reaches into prin-b territory.
	for _, want := range []string{"shared_write", "shared_entity", "cross_border_action"} {
		if !kinds[want] {
			t.Fatalf("missing interaction %q in %+v", want, got)
		}
	}
}

func TestRenderPromptsContainOwnConditionsOnly(t *testing.T) {
	// Guard: the logographer renderer must not print foreign conditions.
	meta := domain.Meta{WorldName: "W", StartDate: "1000-01-01", StepDays: 7}
	v := &knowledge.LogographerView{
		Region:      &domain.Region{ID: "king-a", Name: "Kingdom A", Status: "active", Attrs: map[string]any{"unrest": "low"}},
		Entities:    []*domain.Entity{{ID: "q-elinor", Kind: "person", Name: "Queen Elinor", RegionID: "king-a", Status: "active"}},
		AdjacentIDs: []domain.ID{"prin-b"},
	}
	p := RenderLogographerPrompt(meta, 0, v, 0)
	if !strings.Contains(p, `id=king-a`) || !strings.Contains(p, "unrest") {
		t.Fatal("own conditions missing")
	}
	if strings.Contains(p, `"ambition"`) {
		t.Fatal("foreign conditions leaked into render")
	}
	if !strings.Contains(p, "- prin-b") {
		t.Fatal("neighbour identity should be listed by id only")
	}
}

func TestConsultDocsResolution(t *testing.T) {
	visible := []domain.ReferenceDoc{
		{ID: "ref-a", Scope: "world", Title: "A"},
		{ID: "ref-b", Scope: "world", Title: "B"},
		{ID: "ref-c", Scope: "region", RegionID: "king-a", Title: "C"},
		{ID: "ref-d", Scope: "region", RegionID: "king-a", Title: "D"},
	}
	got := ConsultDocs([]string{"ref-c", "ref-zzz", "ref-c", "ref-a", "ref-b", "ref-d"}, visible)
	if len(got) != 3 {
		t.Fatalf("cap: %+v", got)
	}
	want := []domain.ID{"ref-c", "ref-a", "ref-b"}
	for i := range got {
		if got[i].ID != want[i] {
			t.Errorf("order/dedupe: %+v", got)
			break
		}
	}
	if len(ConsultDocs([]string{"nope"}, visible)) != 0 {
		t.Error("unknown ids must resolve to nothing")
	}
	if section := RenderConsultSection(got); !strings.Contains(section, "REFERENCE DOCUMENTS YOU REQUESTED") {
		t.Errorf("consult section missing: %q", section)
	}
}
