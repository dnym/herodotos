package agents

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
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
	relBad.Origin = "king-a"
	if err := pv.ValidateFinalEvent(relBad); err == nil {
		t.Fatal("out-of-vocabulary status accepted in orchestrator output")
	}
	relGood := &domain.PropEvent{Title: "t", Prose: "p", Origin: "king-a", Changes: []domain.Change{{Object: "relationship", ObjectID: "rel-1", Op: "set_status", Patch: map[string]any{"status": "ended"}}}}
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
	ghostDest.Origin = "king-a"
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
	p := RenderLogographerPrompt(meta, 0, v).String()
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
		`{"events":[]}]`,
		`{"events":[]}}`,
		`{"events":[]}]}`,
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
	contradicting := []ProposalItem{{Ref: "p1", Author: "logographer:king-a", Event: domain.PropEvent{Title: "The healthy queen rides", Prose: "p",
		Changes: []domain.Change{{Object: "entity", ObjectID: "q-elinor", Op: "set_status", Patch: map[string]any{"status": "destroyed"}}}}}}
	p := RenderOrchestratorPrompt(domain.Meta{WorldName: "W"}, 3, snap, contradicting, nil).String()
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
		Observed: map[domain.ID]map[domain.ID]string{
			"king-a": {"evt-000001": "", "evt-000002": ""},
			"prin-b": {"evt-000001": ""},
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

func TestLateReportRevivesOldEventAsCause(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "world.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var w store.WorldSpec
	w.Name = "T"
	w.StartDate = "1000-06-01"
	w.StepDays = 7
	w.Regions = []struct {
		ID    domain.ID      `json:"id"`
		Name  string         `json:"name"`
		Attrs map[string]any `json:"attrs,omitempty"`
	}{{ID: "king-a", Name: "Kingdom A"}, {ID: "prin-b", Name: "Principality B"}}
	if err := db.InitWorld(ctx, &w); err != nil {
		t.Fatalf("init: %v", err)
	}
	att, err := db.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, err := db.CommitStep(ctx, store.CommitPlan{
		AttemptID: att, BaseRevision: 0, Summary: "s1",
		Events: []store.CommittedEvent{{
			Type: "development", Title: "Ford closed", Prose: "Garrisons bar the crossing.",
			RegionIDs: []domain.ID{"prin-b"},
		}},
		Reports: []store.ReportPlan{{EventIndex: 0, TargetRegion: "king-a", EarliestStep: 10}},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The clock is far past the event: it has left the recent window
	// (last 8 steps) and only reaches king-a through the late report.
	for step := 2; step <= 10; step++ {
		att, err := db.BeginAttempt(ctx, step)
		if err != nil {
			t.Fatalf("attempt %d: %v", step, err)
		}
		if _, err := db.CommitStep(ctx, store.CommitPlan{
			AttemptID: att, BaseRevision: int64(step - 1), Summary: "quiet",
		}); err != nil {
			t.Fatalf("commit %d: %v", step, err)
		}
	}
	snap, err := db.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, ev := range snap.RecentEvents {
		if ev.ID == "evt-000001" {
			t.Fatal("fixture broken: event still in recent window")
		}
	}
	var delivered bool
	for _, rep := range snap.DeliveredReports["king-a"] {
		if rep.EventID == "evt-000001" {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("late report not in snapshot knowledge")
	}

	pv := &ProposalValidator{Snap: snap, CurrentStep: 10, DB: db}
	if !pv.causeVisibleTo("king-a", "evt-000001") {
		t.Error("event delivered by late report must be citable by king-a")
	}
	if pv.causeVisibleTo("prin-b", "evt-000001") {
		// prin-b was not the scene region in the recent window here; the
		// fixture marks only king-a as the report target.
		if !snapHasRegionEvent(snap, "prin-b", "evt-000001") {
			t.Error("scene region should still know its own old event")
		}
	}
	if !pv.CanObserveEvent("king-a", "evt-000001") {
		t.Error("reaction premise on a late-reported event must validate")
	}
	// A cause outside the region's knowledge stays rejected even though
	// it canonically exists: existence and access are separate.
	bad := &domain.PropEvent{
		Type: "development", Title: "t", Prose: "p",
		Causes: []domain.ID{"evt-999999"},
	}
	if err := pv.ValidateLogographerProposal("king-a", bad); err == nil {
		t.Error("invented cause accepted")
	}
	orch := &ProposalValidator{Snap: snap, CurrentStep: 10, DB: db, AllEvents: true}
	fin := &domain.PropEvent{
		Type: "development", Title: "t", Prose: "p", Origin: "king-a",
		Causes: []domain.ID{"evt-000001"},
	}
	if err := orch.ValidateFinalEvent(fin); err != nil {
		t.Errorf("orchestrator must cite old canonical events: %v", err)
	}
	inven := *fin
	inven.Causes = []domain.ID{"evt-999999"}
	if err := orch.ValidateFinalEvent(&inven); err == nil {
		t.Error("invented cause accepted by orchestrator")
	}
}

func snapHasRegionEvent(s *store.Snapshot, region string, id domain.ID) bool {
	for _, ev := range s.RecentEvents {
		if ev.ID == id {
			for _, r := range ev.RegionIDs {
				if r == domain.ID(region) {
					return true
				}
			}
		}
	}
	return false
}

func TestBindingRuleBeyondByteCapSurvives(t *testing.T) {
	meta := domain.Meta{WorldName: "W", StartDate: "1000-01-01", StepDays: 7}
	// A binding rule whose critical clause sits well past byte 1,200:
	// the old flat cap cut exactly such rules out of the prompt.
	rule := strings.Repeat("filler ", 400) + "NO IRON MAY CROSS THE FORD."
	v := &knowledge.LogographerView{
		Region: &domain.Region{ID: "king-a", Name: "Kingdom A", Status: "active"},
		Bindings: []domain.ReferenceDoc{
			{ID: "ref-law", Scope: "world", Title: "Ford Law", Binding: true, Body: rule},
		},
		Beliefs:  []domain.Belief{{ID: "bl-1", RegionID: "king-a", Modality: "belief", Statement: strings.Repeat("noise ", 500)}},
		Entities: []*domain.Entity{{ID: "e-1", Kind: "person", Name: strings.Repeat("name ", 300), RegionID: "king-a", Status: "active"}},
	}
	prompt := RenderLogographerPrompt(meta, 0, v)
	p, err := prompt.Fit("", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "NO IRON MAY CROSS THE FORD.") {
		t.Fatal("binding rule beyond byte 1200 was cut from the prompt")
	}
	// Tight budget: optional sections drop first, the binding rule stays.
	p, err = prompt.Fit("", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "NO IRON MAY CROSS THE FORD.") {
		t.Fatal("binding rule dropped under budget pressure")
	}
	if strings.Contains(p, "noise") || strings.Contains(p, "name name") {
		t.Fatal("optional sections should have been dropped first")
	}
	// Absurd budget: a visible budget error, never a trimmed rule.
	var be *BudgetError
	if _, err := prompt.Fit("", 100); !errors.As(err, &be) {
		t.Fatalf("want BudgetError, got %v", err)
	}
}

func TestDeliveredReportsBoundedNewestFirst(t *testing.T) {
	meta := domain.Meta{WorldName: "W", StartDate: "1000-01-01", StepDays: 7}
	v := &knowledge.LogographerView{
		Region:        &domain.Region{ID: "king-a", Name: "Kingdom A", Status: "active"},
		ReportsHidden: 5,
	}
	v.Reports = append(v.Reports, knowledge.ReportNote{
		EventID: "evt-999999", Title: "Fresh critical news", EventStep: 99, DeliveredStep: 140,
	})
	for i := 11; i >= 0; i-- {
		v.Reports = append(v.Reports, knowledge.ReportNote{
			EventID: fmt.Sprintf("evt-%06d", i), Title: fmt.Sprintf("Old %d", i),
			EventStep: i, DeliveredStep: 100 + i,
		})
	}
	p := RenderLogographerPrompt(meta, 140, v).String()
	i := strings.Index(p, "Fresh critical news")
	if i < 0 {
		t.Fatal("newest report missing")
	}
	if j := strings.Index(p, "Old 11"); j >= 0 && j < i {
		t.Errorf("older report (%d) precedes newest (%d)", j, i)
	}
	if !strings.Contains(p, "5 older reports omitted") {
		t.Fatal("hidden-report count missing from heading")
	}
}

func TestFitNeverTrimsMandatoryForAdditions(t *testing.T) {
	meta := domain.Meta{WorldName: "W", StartDate: "1000-01-01", StepDays: 7}
	rule := strings.Repeat("rule ", 100) + "THE FORD IS CLOSED."
	v := &knowledge.LogographerView{
		Region:   &domain.Region{ID: "king-a", Name: "Kingdom A", Status: "active"},
		Bindings: []domain.ReferenceDoc{{ID: "ref-law", Scope: "world", Title: "Ford Law", Binding: true, Body: rule}},
		Entities: []*domain.Entity{{ID: "e-1", Kind: "person", Name: strings.Repeat("name ", 100), RegionID: "king-a", Status: "active"}},
	}
	prompt := RenderLogographerPrompt(meta, 0, v)
	full := prompt.String()
	extra := "\nCORRECTION: " + strings.Repeat("x", 300) + "\n"
	// Room for the rule and the addition, not for the optional entities.
	got, err := prompt.Fit(extra, len(full)+len(extra)-200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "THE FORD IS CLOSED.") || !strings.Contains(got, "CORRECTION") {
		t.Fatal("rule or addition lost")
	}
	if strings.Contains(got, "name name") {
		t.Fatal("optional section should make room first")
	}
	// An addition too large for the remaining room is an error, and
	// the rule is never sacrificed for it.
	huge := strings.Repeat("y", len(full))
	var be *BudgetError
	if _, err := prompt.Fit(huge, len(full)); !errors.As(err, &be) {
		t.Fatalf("want BudgetError, got %v", err)
	}
}
