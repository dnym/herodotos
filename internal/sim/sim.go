// Package sim owns the step lifecycle: snapshot, independent proposals,
// interaction detection, adjudication, and atomic commitment. Agents
// propose; only this package commits.
package sim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dnym/herodotos/internal/agents"
	"github.com/dnym/herodotos/internal/config"
	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/inference"
	"github.com/dnym/herodotos/internal/knowledge"
	"github.com/dnym/herodotos/internal/store"
)

// Engine runs simulation steps against one database.
type Engine struct {
	DB        *store.DB
	Client    inference.Client
	ModelName string // recorded for auditability
	Budgets   config.Budgets
}

// Result describes one committed step.
type Result struct {
	AttemptID  string
	Step       int
	EventIDs   []string
	Notes      string
	Proposals  int
	ModelCalls int
}

// Failure reports an attempt that produced no canonical change.
type Failure struct {
	AttemptID string
	Reason    string
	Err       error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return fmt.Sprintf("step not committed (attempt %s): %s: %v", f.AttemptID, f.Reason, f.Err)
	}
	return fmt.Sprintf("step not committed (attempt %s): %s", f.AttemptID, f.Reason)
}

// Unwrap exposes the underlying cause.
func (f *Failure) Unwrap() error { return f.Err }

// ErrPaused means the world is in the durable paused state: a human must
// resolve the conflict (usually a hard directive) before steps continue.
var ErrPaused = errors.New("world is paused")

// RunStep advances the world by exactly one step. On any failure the
// attempt keeps its diagnostics but the canonical state and clock are
// untouched.
func (e *Engine) RunStep(ctx context.Context) (*Result, error) {
	if e.Client == nil {
		return nil, fmt.Errorf("sim: no inference client configured")
	}
	b := e.Budgets
	if b.MaxModelCallsPerStep <= 0 {
		b = config.DefaultBudgets()
	}

	// A durable pause blocks before any attempt opens: the halt is world
	// state, not process state.
	if reason, paused, err := e.DB.WorldPause(ctx); err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	} else if paused {
		return nil, fmt.Errorf("%w: %s (resolve the conflict, then resume)", ErrPaused, reason)
	}

	// Open the attempt first: BeginAttempt marks this boundary's report
	// deliveries durably, so the snapshot below (and every agent view
	// built from it) already sees news that has now arrived.
	meta, err := e.DB.GetMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}
	newStep := meta.ClockStep + 1
	attemptID, err := e.DB.BeginAttempt(ctx, newStep)
	if err != nil {
		return nil, fmt.Errorf("begin attempt: %w", err)
	}

	snap, err := e.DB.LoadSnapshot(ctx)
	if err != nil {
		e.DB.MarkAttemptFailed(context.WithoutCancel(ctx), attemptID, "snapshot failed")
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	currentStep := snap.Meta.ClockStep

	fail := func(reason string, err error) (*Result, error) {
		e.DB.MarkAttemptFailed(context.WithoutCancel(ctx), attemptID, reason)
		return nil, &Failure{AttemptID: attemptID, Reason: reason, Err: err}
	}

	st := &stepRun{e: e, b: b, attempt: attemptID, snap: snap, pv: agents.ProposalValidator{
		Snap: snap, CurrentStep: currentStep,
	}}

	proposals := map[string][]domain.PropEvent{}
	memories := map[string]*agents.MemoryUpdate{}
	beliefs := map[string][]agents.BeliefRecord{}
	propSeq := 0
	recordProposal := func(author, kind, proseText string, evs []domain.PropEvent) error {
		propSeq++
		return e.DB.RecordProposal(ctx, attemptID, domain.Proposal{
			ID: fmt.Sprintf("%s-p%03d", attemptID, propSeq), Author: author, Kind: kind,
			Prose: proseText, Events: evs,
		})
	}
	regionIDs := make([]string, 0, len(snap.Regions))
	for id, r := range snap.Regions {
		if r.Status == "active" {
			regionIDs = append(regionIDs, id)
		}
	}
	sort.Strings(regionIDs)

	for _, regionID := range regionIDs {
		view, err := knowledge.BuildLogographerView(ctx, snap, regionID, e.DB)
		if err != nil {
			return fail("build view", err)
		}
		reply, err := st.askLogographer(ctx, regionID, view, "", nil)
		if err != nil {
			return fail(fmt.Sprintf("region %s proposal failed", regionID), err)
		}
		// Skill-style progressive disclosure: if the historian asked
		// for cataloged documents, spend exactly one follow-up call
		// with their texts in context. A failed consult costs a call
		// but never the step; the first reply stands and there is no
		// second consult round.
		if len(reply.Consult) > 0 && st.used < b.MaxModelCallsPerStep {
			if docs := agents.ConsultDocs(reply.Consult, knowledge.VisibleReferences(view)); len(docs) > 0 {
				if consulted, cerr := st.askConsult(ctx, regionID, view, docs); cerr == nil {
					reply = consulted
				}
			}
		}
		author := "logographer:" + regionID
		if len(reply.Events) > 0 {
			proposals[author] = append(proposals[author], reply.Events...)
		}
		if reply.Memory != nil && reply.Memory.Summary != "" {
			memories[regionID] = reply.Memory
		}
		if len(reply.Beliefs) > 0 {
			beliefs[regionID] = append(beliefs[regionID], reply.Beliefs...)
		}
		kind := "development"
		if len(reply.Events) == 0 {
			kind = "none"
		}
		if err := recordProposal(author, kind, reply.Prose, reply.Events); err != nil {
			return fail("record proposal", err)
		}
	}

	var final *agents.OrchestratorReply
	for round := 0; ; round++ {
		interactions := agents.DetectInteractions(proposals)
		prompt := agents.RenderOrchestratorPrompt(snap.Meta, currentStep, snap, proposals, interactions, b.MaxPromptBytes)
		reply, err := st.askOrchestrator(ctx, prompt, round < b.MaxReactionRounds)
		if err != nil {
			return fail("orchestrator failed", err)
		}

		if reply.Pause != nil {
			reason := reply.Pause.Reason
			if reason == "" {
				reason = "paused for clarification"
			}
			if reply.Pause.DirectiveID != "" {
				reason += fmt.Sprintf(" (directive %s)", reply.Pause.DirectiveID)
			}
			// The halt is durable world state: it must survive restarts
			// and block later steps until a human resumes. The attempt
			// still produces zero canonical changes.
			if err := e.DB.PauseWorld(context.WithoutCancel(ctx), attemptID, reason); err != nil {
				return fail("record pause", err)
			}
			return fail(reason, nil)
		}
		if len(reply.Reactions) == 0 || round >= b.MaxReactionRounds {
			final = reply
			break
		}
		regions := make([]string, 0, len(reply.Reactions))
		for r := range reply.Reactions {
			regions = append(regions, r)
		}
		sort.Strings(regions)
		for _, regionID := range regions {
			if snap.Regions[regionID] == nil {
				continue
			}
			view, err := knowledge.BuildLogographerView(ctx, snap, regionID, e.DB)
			if err != nil {
				return fail("build reaction view", err)
			}
			question := reply.Reactions[regionID].Question
			var premise []domain.Event
			for _, id := range reply.Reactions[regionID].Premise {
				for _, ev := range snap.RecentEvents {
					if ev.ID == id {
						premise = append(premise, ev)
						break
					}
				}
			}
			reply2, err := st.askLogographer(ctx, regionID, view, question, premise)
			if err != nil {
				return fail(fmt.Sprintf("region %s reaction failed", regionID), err)
			}
			author := "logographer:" + regionID
			if len(reply2.Events) > 0 {
				proposals[author] = append(proposals[author], reply2.Events...)
			}
			if reply2.Memory != nil && reply2.Memory.Summary != "" {
				memories[regionID] = reply2.Memory
			}
			if len(reply2.Beliefs) > 0 {
				beliefs[regionID] = append(beliefs[regionID], reply2.Beliefs...)
			}
			kind := "reaction"
			if len(reply2.Events) == 0 {
				kind = "none"
			}
			if err := recordProposal(author, kind, reply2.Prose, reply2.Events); err != nil {
				return fail("record reaction", err)
			}
		}
	}

	plan := store.CommitPlan{
		AttemptID:    attemptID,
		BaseRevision: snap.Meta.Revision,
		Summary:      summarize(final.Notes),
	}
	// Which regions survive as authors in the adjudicated outcome.
	represented := map[domain.ID]bool{}
	for _, ev := range final.Events {
		if err := st.pv.ValidateFinalEvent(&ev); err != nil {
			return fail("final event rejected", err)
		}
		if ev.Origin == "" {
			ev.Origin = "orchestrator"
		}
		if ev.Origin != "orchestrator" {
			represented[ev.Origin] = true
		}
		ce := store.CommittedEvent{
			Type: ev.Type, Title: ev.Title, Prose: ev.Prose,
			Changes:   ev.Changes,
			EntityIDs: ev.AffectsEntities,
			CauseIDs:  ev.Causes,
		}
		ce.RegionIDs = affectedRegions(snap, &ev)
		idx := len(plan.Events)
		plan.Events = append(plan.Events, ce)
		for region, step := range ev.Delivery {
			if region == ev.Origin {
				continue
			}
			plan.Reports = append(plan.Reports, store.ReportPlan{EventIndex: idx, TargetRegion: region, EarliestStep: step})
		}
	}

	if len(final.DirectiveStatus) > 0 {
		changes := make([]domain.Change, 0, len(final.DirectiveStatus))
		for _, ds := range final.DirectiveStatus {
			if snap.Directives[ds.ID] == nil {
				return fail("final event rejected", fmt.Errorf("unknown directive %q", ds.ID))
			}
			changes = append(changes, domain.Change{
				Object: "directive", ObjectID: ds.ID, Op: "set_status",
				Patch: map[string]any{"status": ds.Status},
			})
		}
		plan.Events = append(plan.Events, store.CommittedEvent{
			Type: "resolution", Title: "Directive review",
			Prose:   "The orchestrator reviewed standing directives.",
			Changes: changes,
		})
	}

	for regionID, m := range memories {
		// Rejected fiction must not survive as invented history: when a
		// region proposed events but none of them survived adjudication
		// under its authorship, its memory update describes outcomes
		// that never happened — keep the previous memory instead. A
		// quiet step's assessments (no proposals) always stand; beliefs
		// are in-world opinions and stand on their own.
		if len(proposals["logographer:"+regionID]) > 0 && !represented[regionID] {
			continue
		}
		plan.Memories = append(plan.Memories, domain.Memory{
			RegionID: regionID, Summary: m.Summary, OpenThreads: m.OpenThreads, UpdatedStep: newStep,
		})
	}

	for regionID, bs := range beliefs {
		for _, bl := range bs {
			plan.Beliefs = append(plan.Beliefs, domain.Belief{
				RegionID: regionID, SubjectID: bl.SubjectID,
				Modality: bl.Modality, Statement: bl.Statement,
			})
		}
	}

	ids, err := e.DB.CommitStep(ctx, plan)
	if err != nil {
		return fail("commit", err)
	}
	return &Result{
		AttemptID:  attemptID,
		Step:       newStep,
		EventIDs:   ids,
		Notes:      final.Notes,
		Proposals:  countProposals(proposals),
		ModelCalls: st.used,
	}, nil
}

func countProposals(p map[string][]domain.PropEvent) int {
	n := 0
	for _, evs := range p {
		n += len(evs)
	}
	return n
}

// affectedRegions derives the regions an event record should list: the
// origin plus every region whose state or entities the event touches.
func affectedRegions(snap *store.Snapshot, ev *domain.PropEvent) []domain.ID {
	seen := map[domain.ID]bool{}
	var out []domain.ID
	add := func(id domain.ID) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if ev.Origin != "orchestrator" {
		add(ev.Origin)
	}
	for _, ch := range ev.Changes {
		switch ch.Object {
		case "region":
			add(ch.ObjectID)
		case "entity":
			if e := snap.Entities[ch.ObjectID]; e != nil {
				add(e.RegionID)
			}
			if rid, _ := ch.Patch["region_id"].(string); rid != "" {
				add(rid)
			}
		}
	}
	for _, id := range ev.AffectsEntities {
		if e := snap.Entities[id]; e != nil {
			add(e.RegionID)
		}
	}
	sort.Strings(out)
	return out
}

func summarize(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// stepRun carries the mutable state of one attempt.
type stepRun struct {
	e       *Engine
	b       config.Budgets
	attempt string
	snap    *store.Snapshot
	pv      agents.ProposalValidator
	used    int
}

func (s *stepRun) askLogographer(ctx context.Context, regionID string, view *knowledge.LogographerView, extra string, premise []domain.Event) (*agents.LogographerReply, error) {
	prompt := agents.RenderLogographerPrompt(s.snap.Meta, s.snap.Meta.ClockStep, view, s.b.MaxPromptBytes)
	if extra != "" {
		prompt += "\nTHE ORCHESTRATOR ASKS YOU: " + extra + "\n"
		if len(premise) > 0 {
			prompt += "The question rests on events you can see in your own history or delivered reports:\n"
			for _, ev := range premise {
				prompt += fmt.Sprintf("- %s step %d: %s: %s\n", ev.ID, ev.Step, ev.Title, ev.Prose)
			}
		}
		prompt += "Answer with the same JSON format, reacting only within your authority.\n"
	}
	var reply *agents.LogographerReply
	err := retryParse(s, ctx, "logographer:"+regionID, agents.SystemLogographer(), prompt, 0.8, s.b.MaxOutputTokens,
		func(text string) error {
			r, err := agents.ParseLogographerReply(text)
			if err != nil {
				return err
			}
			for i := range r.Events {
				if err := s.pv.ValidateLogographerProposal(regionID, &r.Events[i]); err != nil {
					return err
				}
			}
			for _, bl := range r.Beliefs {
				if bl.SubjectID != "" && s.snap.Entities[bl.SubjectID] == nil {
					return fmt.Errorf("belief subject %q is not a known entity", bl.SubjectID)
				}
			}
			reply = r
			return nil
		})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

func (s *stepRun) askConsult(ctx context.Context, regionID string, view *knowledge.LogographerView, docs []domain.ReferenceDoc) (*agents.LogographerReply, error) {
	prompt := agents.RenderLogographerPrompt(s.snap.Meta, s.snap.Meta.ClockStep, view, s.b.MaxPromptBytes)
	prompt += agents.RenderConsultSection(docs) +
		"\nThese are the reference documents you asked for. Give your final answer now in the same JSON format; no further documents are available this step.\n"
	var reply *agents.LogographerReply
	err := retryParse(s, ctx, "logographer:"+regionID+":consult", agents.SystemLogographer(), prompt, 0.8, s.b.MaxOutputTokens,
		func(text string) error {
			r, err := agents.ParseLogographerReply(text)
			if err != nil {
				return err
			}
			for i := range r.Events {
				if err := s.pv.ValidateLogographerProposal(regionID, &r.Events[i]); err != nil {
					return err
				}
			}
			for _, bl := range r.Beliefs {
				if bl.SubjectID != "" && s.snap.Entities[bl.SubjectID] == nil {
					return fmt.Errorf("belief subject %q is not a known entity", bl.SubjectID)
				}
			}
			reply = r
			return nil
		})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

func (s *stepRun) askOrchestrator(ctx context.Context, prompt string, reactionsOK bool) (*agents.OrchestratorReply, error) {
	var reply *agents.OrchestratorReply
	maxTok := s.b.MaxOutputTokensOrchestr
	if maxTok <= 0 {
		maxTok = 2 * s.b.MaxOutputTokens
	}
	err := retryParse(s, ctx, "orchestrator", agents.SystemOrchestrator(), prompt, 0.3, maxTok,
		func(text string) error {
			r, err := agents.ParseOrchestratorReply(text)
			if err != nil {
				return err
			}
			// Validate the merged event set here so id mistakes go back
			// to the orchestrator as corrections, not as a dead attempt.
			for i := range r.Events {
				if err := s.pv.ValidateFinalEvent(&r.Events[i]); err != nil {
					return err
				}
			}
			for _, ds := range r.DirectiveStatus {
				if s.snap.Directives[ds.ID] == nil {
					return fmt.Errorf("unknown directive %q in directive_status", ds.ID)
				}
			}
			if reactionsOK {
				for region, rx := range r.Reactions {
					if err := s.pv.ValidateReaction(domain.ID(region), rx); err != nil {
						return err
					}
				}
			}
			reply = r
			return nil
		})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// retryParse calls the model and feeds structured failures back for a
// bounded number of correction rounds.
func retryParse(s *stepRun, ctx context.Context, role, system, prompt string, temp float64, maxTokens int, validate func(string) error) error {
	correction := ""
	var lastErr error
	for tries := 0; tries <= s.b.MaxCorrectionRetries; tries++ {
		text, truncated, err := s.call(ctx, role, system, prompt+correction, temp, maxTokens)
		if err != nil {
			return err
		}
		if truncated {
			lastErr = fmt.Errorf("reply hit the %d-token output limit and is incomplete", maxTokens)
		} else {
			lastErr = validate(text)
		}
		if lastErr == nil {
			return nil
		}
		diagnosis := lastErr.Error()
		if truncated {
			diagnosis = "your reply was cut off at the output limit, so the JSON is incomplete. " +
				"Reply again with at most half the detail: fewer events, two short sentences per event prose, " +
				"minimal patches. A complete smaller reply beats a rich truncated one."
		}
		correction = "\n\nYour previous reply was rejected: " + diagnosis + "\nReply with only a corrected JSON object."
	}
	return fmt.Errorf("exhausted %d correction retries (last: %v)", s.b.MaxCorrectionRetries, lastErr)
}

// call performs one model interaction inside the per-step budget.
// maxTokens <= 0 falls back to the logographer ceiling. The bool result
// reports whether the backend cut the reply at the output limit.
func (s *stepRun) call(ctx context.Context, role, system, prompt string, temp float64, maxTokens int) (string, bool, error) {
	s.used++
	if s.used > s.b.MaxModelCallsPerStep {
		return "", false, fmt.Errorf("model call budget (%d) exhausted", s.b.MaxModelCallsPerStep)
	}
	if maxTokens <= 0 {
		maxTokens = s.b.MaxOutputTokens
	}
	start := time.Now()
	resp, callErr := s.e.Client.Complete(ctx, inference.Request{
		Role: role, System: system, Prompt: prompt,
		MaxTokens: maxTokens, Temperature: temp,
	})
	dur := int(time.Since(start).Milliseconds())

	sum := sha256.Sum256([]byte(system + "\x00" + prompt))
	reqJSON := jsonPreview(map[string]string{"system": system, "prompt": prompt})
	var respJSON string
	var truncNote string
	if callErr == nil {
		respJSON = jsonPreview(map[string]string{"text": resp.Text})
		if resp.Truncated {
			truncNote = "reply truncated at output limit"
		}
	}
	errStr := truncNote
	if callErr != nil {
		errStr = callErr.Error()
	}
	if recErr := s.e.DB.RecordModelCall(ctx, s.attempt, role, s.e.ModelName, hex.EncodeToString(sum[:]),
		reqJSON, respJSON, resp.InputTokens, resp.OutputTokens, dur, errStr); recErr != nil {
		return "", false, fmt.Errorf("record model call: %w", recErr)
	}
	if callErr != nil {
		return "", false, fmt.Errorf("inference call for %s: %w", role, callErr)
	}
	return resp.Text, resp.Truncated, nil
}

func jsonPreview(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	if len(b) > 120_000 {
		return string(b[:120_000]) + "...[truncated]"
	}
	return string(b)
}
