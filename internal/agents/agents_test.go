package agents

import (
	"math/rand"
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

	// Movement: upsert may move an entity of yours to an existing
	// region; set_attrs may not pose as movement.
	moveOK := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "entity", ObjectID: "q-elinor", Op: "upsert", Patch: map[string]any{"region_id": "prin-b"}}}}
	if err := pv.ValidateLogographerProposal("king-a", moveOK); err != nil {
		t.Errorf("valid move rejected: %v", err)
	}
	fakeMove := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "entity", ObjectID: "q-elinor", Op: "set_attrs", Patch: map[string]any{"region_id": "prin-b"}}}}
	err = pv.ValidateLogographerProposal("king-a", fakeMove)
	if err == nil {
		t.Fatal("set_attrs with region_id accepted")
	}
	if !strings.Contains(err.Error(), "use upsert to move") {
		t.Errorf("rejection must teach the mechanism: %v", err)
	}
	ghostDest := &domain.PropEvent{Title: "t", Prose: "p", Changes: []domain.Change{{Object: "entity", ObjectID: "q-elinor", Op: "upsert", Patch: map[string]any{"region_id": "nowhere"}}}}
	if err := pv.ValidateLogographerProposal("king-a", ghostDest); err == nil {
		t.Fatal("move to unknown region accepted")
	}
	if err := pv.ValidateFinalEvent(ghostDest); err == nil {
		t.Fatal("move to unknown region accepted in final event")
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

func TestMalformedRepliesNeverPanic(t *testing.T) {
	// Anything a truncated or confused model can emit must surface as a
	// parse error for the correction round, never as a panic, and never
	// as a successful decode of garbage.
	inputs := []string{
		"", " ", "```", "```json", "```", "\n", "```{", "```{bad json",
		"{", "{not json}", "[]", "null", "42", `"text reply"`,
		`{"prose":"ok"}{"prose":"second"}`,
		`{"prose":"ok"} trailing narration`,
		`{"prose":"ok", "deliveries": {}}`,
		"```json\n{\"prose\":\"fenced\",\"events\":[]}\n```",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic on input %q: %v", in, p)
				}
			}()
			_, err := ParseLogographerReply(in)
			wantOK := in == "```json\n{\"prose\":\"fenced\",\"events\":[]}\n```"
			if wantOK && err != nil {
				t.Errorf("valid fenced reply rejected: %v", err)
			}
			if !wantOK && err == nil {
				t.Errorf("malformed reply accepted: %q", in)
			}
		})
	}
}

func TestFuzzParseNeverPanics(t *testing.T) {
	// Deterministic pseudo-fuzz: random noise from a fixed seed must
	// produce errors, never panics.
	alphabet := []rune("{}[]\"`,:nulstre12 \n\t\\")
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		b := make([]rune, 1+rng.Intn(30))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		s := string(b)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic on input %q: %v", s, p)
				}
			}()
			_, _ = ParseLogographerReply(s)
			_, _ = ParseOrchestratorReply(s)
		}()
	}
}

func TestOrchestratorPromptCarriesCanonicalEntityState(t *testing.T) {
	// The adjudicator must be able to inspect established entity and
	// relationship records, not just what proposals choose to restate.
	snap := &store.Snapshot{
		Regions: map[domain.ID]*domain.Region{
			"king-a": {ID: "king-a", Name: "King A", Status: "active"},
			"prin-b": {ID: "prin-b", Name: "Prin B", Status: "active"},
		},
		Entities: map[domain.ID]*domain.Entity{
			"q-elinor": {ID: "q-elinor", Kind: "person", Name: "Queen Elinor",
				RegionID: "king-a", Status: "active",
				Attrs: map[string]any{"health": "failing", "claims": 2}},
		},
		Relationships: map[domain.ID]*domain.Relationship{
			"rel-a-c": {ID: "rel-a-c", SourceID: "king-a", TargetID: "prin-b",
				Kind: "alliance", Status: "active"},
		},
	}
	contradicting := map[string][]domain.PropEvent{
		"logographer:king-a": {{Title: "The healthy queen rides", Prose: "p",
			Changes: []domain.Change{{Object: "entity", ObjectID: "q-elinor", Op: "set_status", Patch: map[string]any{"status": "destroyed"}}}}},
	}
	p := RenderOrchestratorPrompt(domain.Meta{WorldName: "W"}, 3, snap, contradicting, nil, 0)
	for _, want := range []string{
		"ENTITIES", "q-elinor", "health", "failing",
		"region=king-a", "status=active",
		"RELATIONSHIPS", "rel-a-c", "alliance",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("orchestrator prompt missing %q", want)
		}
	}
	if !strings.Contains(p, "canonical") {
		t.Error("canonical records must be labeled as such, distinct from proposals")
	}
}

func TestLegacyStringReactionRejected(t *testing.T) {
	// The old free-form "reactions": {"region": "question"} form must
	// fail with a parse error that teaches the structured envelope,
	// not silently pass.
	_, err := ParseOrchestratorReply(`{"notes":"n","events":[],"reactions":{"prin-b":"why?"}}`)
	if err == nil {
		t.Fatal("string reaction accepted")
	}
	if !strings.Contains(err.Error(), "question") || !strings.Contains(err.Error(), "premise") {
		t.Errorf("parse error must teach the envelope: %v", err)
	}
	if _, err := ParseOrchestratorReply(`{"notes":"n","events":[],"reactions":{"prin-b":{"question":"why?","premise":["evt-000001"]}}}`); err != nil {
		t.Errorf("valid structured reaction rejected: %v", err)
	}
	if _, err := ParseOrchestratorReply(`{"notes":"n","events":[],"reactions":{"prin-b":{"question":"","premise":["evt-000001"]}}}`); err == nil {
		t.Error("empty question accepted")
	}
}

func TestReactionPremiseVisibility(t *testing.T) {
	snap := &store.Snapshot{
		Regions: map[domain.ID]*domain.Region{"king-a": {ID: "king-a"}, "prin-b": {ID: "prin-b"}},
		RecentEvents: []domain.Event{
			{ID: "evt-000001", Step: 1, Title: "Clash", RegionIDs: []domain.ID{"king-a", "prin-b"}},
			{ID: "evt-000002", Step: 1, Title: "Riot", RegionIDs: []domain.ID{"king-a"}},
		},
		DeliveredReports: map[domain.ID][]domain.Report{
			"prin-b": {{EventID: "evt-000002", EarliestStep: 2, DeliveredStep: 2}},
		},
	}
	pv := &ProposalValidator{Snap: snap, CurrentStep: 2}
	if err := pv.ValidateReaction("prin-b", ReactionRequest{Question: "q", Premise: []domain.ID{"evt-000001"}}); err != nil {
		t.Errorf("involved event rejected: %v", err)
	}
	if err := pv.ValidateReaction("prin-b", ReactionRequest{Question: "q", Premise: []domain.ID{"evt-000002"}}); err != nil {
		t.Errorf("delivered report event rejected: %v", err)
	}
	if err := pv.ValidateReaction("king-a", ReactionRequest{Question: "q", Premise: []domain.ID{"evt-000002"}}); err != nil {
		t.Errorf("own event rejected: %v", err)
	}
	bad := pv.ValidateReaction("prin-b", ReactionRequest{Question: "q", Premise: []domain.ID{"evt-999999"}})
	if bad == nil {
		t.Fatal("unknown premise event accepted")
	}
	if !strings.Contains(bad.Error(), "not knowable") {
		t.Errorf("error must teach visibility rules: %v", bad)
	}
	if err := pv.ValidateReaction("prin-b", ReactionRequest{Question: "q"}); err == nil {
		t.Error("premise-less reaction accepted")
	}
	if err := pv.ValidateReaction("nowhere", ReactionRequest{Question: "q", Premise: []domain.ID{"evt-000001"}}); err == nil {
		t.Error("unknown reaction target accepted")
	}
}
