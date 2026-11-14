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

	// Open the attempt first so its diagnostics row exists before any
	// work begins. Report deliveries are applied to the snapshot
	// provisionally for this step (LoadSnapshotForAttempt) and marked
	// durably only when the step commits.
	meta, err := e.DB.GetMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("meta: %w", err)
	}
	newStep := meta.ClockStep + 1
	attemptID, err := e.DB.BeginAttempt(ctx, newStep)
	if err != nil {
		return nil, fmt.Errorf("begin attempt: %w", err)
	}

	snap, err := e.DB.LoadSnapshotForAttempt(ctx, newStep)
	if err != nil {
		e.DB.MarkAttemptFailed(context.WithoutCancel(ctx), attemptID, "snapshot failed", err.Error())
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	currentStep := snap.Meta.ClockStep

	fail := func(reason string, err error) (*Result, error) {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		e.DB.MarkAttemptFailed(context.WithoutCancel(ctx), attemptID, reason, detail)
		return nil, &Failure{AttemptID: attemptID, Reason: reason, Err: err}
	}

	st := &stepRun{e: e, b: b, attempt: attemptID, snap: snap, pv: agents.ProposalValidator{
		Snap: snap, CurrentStep: currentStep, DB: e.DB, AllEvents: true,
	}}
	st.viewPV = st.pv
	st.viewPV.AllEvents = false

	var items []agents.ProposalItem
	addItems := func(author string, evs []domain.PropEvent) {
		for _, ev := range evs {
			items = append(items, agents.ProposalItem{Ref: fmt.Sprintf("p%d", len(items)+1), Author: author, Event: ev})
		}
	}
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
		reply, err := st.askLogographer(ctx, regionID, view, "")
		if err != nil {
			return fail(fmt.Sprintf("region %s proposal failed", regionID), err)
		}
		// Skill-style progressive disclosure: if the historian asked
		// for cataloged documents or for its own older history, spend
		// exactly one follow-up call with those texts in context. A
		// failed follow-up costs a call but never the step; the first
		// reply stands and there is no second round.
		if (len(reply.Consult) > 0 || len(reply.Recall) > 0) && st.used < b.MaxModelCallsPerStep {
			docs := agents.ConsultDocs(reply.Consult, knowledge.VisibleReferences(view))
			var add string
			if len(docs) > 0 {
				add += agents.RenderConsultSection(docs)
			}
			if len(reply.Recall) > 0 {
				history, herr := e.DB.RegionHistory(ctx, regionID, newStep)
				if herr != nil {
					return fail("history lookup", herr)
				}
				add += agents.RenderRecallSection(reply.Recall, knowledge.Recall(history, reply.Recall))
			}
			if add != "" {
				if followed, ferr := st.askFollowUp(ctx, regionID, view, add); ferr == nil {
					reply = followed
				}
			}
		}
		author := "logographer:" + regionID
		addItems(author, reply.Events)
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

	// shown records, per region, the provisional encounters a reaction
	// request disclosed; memory written after seeing them depends on
	// how they are adjudicated.
	shown := map[domain.ID][]string{}
	var final *agents.OrchestratorReply
	for round := 0; ; round++ {
		interactions := agents.DetectInteractions(agents.ProposalMap(items))
		prompt := agents.RenderOrchestratorPrompt(snap.Meta, currentStep, snap, items, interactions)
		// Once the reaction budget is spent, exactly one final
		// adjudication call remains: it must resolve or pause. An
		// unresolved reaction request is never a successful outcome.
		finalOnly := round >= b.MaxReactionRounds
		reply, err := st.askOrchestrator(ctx, prompt, !finalOnly, finalOnly, items)
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
			rx := reply.Reactions[regionID]
			var evidence agents.ReactionEvidence
			for _, id := range rx.Premise {
				ev, gerr := e.DB.GetEvent(ctx, id)
				if gerr != nil {
					return fail("load reaction premise", gerr)
				}
				account, _ := knowledge.AccountFor(snap, domain.ID(regionID), id)
				evidence.Known = append(evidence.Known, knowledge.AsKnown(ev, account))
			}
			for _, ref := range rx.Encounters {
				for _, it := range items {
					if it.Ref != ref {
						continue
					}
					seen := domain.Event{Title: it.Event.Title, Prose: it.Event.Prose}
					if a := it.Event.Accounts[regionID]; a != "" {
						seen = domain.Event{Title: "What your people see", Prose: a}
					}
					evidence.Encounters = append(evidence.Encounters, seen)
					shown[domain.ID(regionID)] = append(shown[domain.ID(regionID)], ref)
				}
			}
			reply2, err := st.askLogographer(ctx, regionID, view, agents.RenderReactionRequest(evidence))
			if err != nil {
				return fail(fmt.Sprintf("region %s reaction failed", regionID), err)
			}
			author := "logographer:" + regionID
			addItems(author, reply2.Events)
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
	if err := st.pv.ValidateFinalEvents(final.Events); err != nil {
		return fail("final event rejected", err)
	}
	for _, ev := range final.Events {
		if ev.Origin == "" {
			ev.Origin = "orchestrator"
		}
		ce := store.CommittedEvent{
			Type: ev.Type, Title: ev.Title, Prose: ev.Prose,
			Changes:   ev.Changes,
			EntityIDs: ev.AffectsEntities,
			CauseIDs:  ev.Causes,
		}
		ce.RegionIDs = affectedRegions(snap, &ev)
		witnesses := observers(&ev)
		for _, r := range witnesses {
			ce.Observers = append(ce.Observers, store.Observation{Region: r, Account: ev.Accounts[r]})
		}
		idx := len(plan.Events)
		plan.Events = append(plan.Events, ce)
		for _, region := range sortedKeys(ev.Delivery) {
			if contains(witnesses, region) {
				continue // it saw for itself; no news needs to travel
			}
			plan.Reports = append(plan.Reports, store.ReportPlan{
				EventIndex: idx, TargetRegion: region, EarliestStep: ev.Delivery[region], Account: ev.Accounts[region],
				Mechanism: ev.Expedited[region],
			})
		}
	}

	if len(final.DirectiveStatus) > 0 {
		changes := make([]domain.Change, 0, len(final.DirectiveStatus))
		for _, ds := range final.DirectiveStatus {
			if snap.Directives[ds.ID] == nil {
				return fail("final event rejected", fmt.Errorf("unknown directive %q", ds.ID))
			}
			// Re-listing a status that has not changed is pure
			// bookkeeping: it must not enter the event stream, where it
			// would crowd the narrative timeline and pose as a cause.
			if snap.Directives[ds.ID].Status == ds.Status {
				continue
			}
			changes = append(changes, domain.Change{
				Object: "directive", ObjectID: ds.ID, Op: "set_status",
				Patch: map[string]any{"status": ds.Status},
			})
		}
		if len(changes) > 0 {
			plan.Events = append(plan.Events, store.CommittedEvent{
				Type: "resolution", Title: "Directive review",
				Prose:   "The orchestrator reviewed standing directives.",
				Changes: changes,
			})
		}
	}

	// Memory and beliefs written before adjudication can only be
	// trusted where adjudication kept the region's proposals (and any
	// encounters it was shown) unchanged. Everywhere else the region
	// rewrites them once against the committed outcome — a bounded
	// call; if it cannot be made, the previous memory stands and the
	// step's beliefs are dropped. A quiet step's assessments (no
	// proposals, no encounters) always stand.
	outcomes := agents.ResolveOutcomes(items, final.Events, final.Dispositions)
	if err := e.DB.RecordProposalOutcomes(context.WithoutCancel(ctx), attemptID, outcomes); err != nil {
		return fail("record proposal outcomes", err)
	}
	byRef := map[string]store.ProposalOutcome{}
	for _, o := range outcomes {
		byRef[o.Ref] = o
	}
	for _, regionID := range regionIDs {
		draft, drafted := memories[regionID], beliefs[regionID]
		if draft == nil && len(drafted) == 0 {
			continue
		}
		in := agents.ReconcileInput{Draft: draft, Beliefs: drafted}
		settled := true
		for _, o := range outcomes {
			if o.Author == "logographer:"+regionID {
				in.Own = append(in.Own, o)
				settled = settled && o.Outcome == "accepted"
			}
		}
		for _, ref := range shown[domain.ID(regionID)] {
			o := byRef[ref]
			in.Encounters = append(in.Encounters, o)
			settled = settled && o.Outcome == "accepted"
		}
		if !settled {
			for _, fe := range final.Events {
				for _, ob := range observers(&fe) {
					if ob == regionID {
						in.Witnessed = append(in.Witnessed, domain.Event{Title: fe.Title, Prose: fe.Prose})
						if a := fe.Accounts[regionID]; a != "" {
							in.Witnessed[len(in.Witnessed)-1] = domain.Event{Title: "What your region learned", Prose: a}
						}
					}
				}
			}
			view, verr := knowledge.BuildLogographerView(ctx, snap, regionID, e.DB)
			if verr != nil {
				return fail("build reconcile view", verr)
			}
			rec, rerr := st.askReconcile(ctx, regionID, view, in, final.Events)
			if rerr != nil {
				continue // conservative: previous memory stands, beliefs dropped
			}
			draft, drafted = rec.Memory, rec.Beliefs
		}
		if draft != nil && draft.Summary != "" {
			plan.Memories = append(plan.Memories, domain.Memory{
				RegionID: regionID, Summary: draft.Summary, OpenThreads: draft.OpenThreads, UpdatedStep: newStep,
			})
		}
		for _, bl := range drafted {
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
		Proposals:  len(items),
		ModelCalls: st.used,
	}, nil
}

// observers lists the regions that witness an event directly: the
// regional origin, which always sees its own event in full, plus the
// declared observers. Affected regions are deliberately not included —
// a target of a covert act learns of it only by observation or report.
func observers(ev *domain.PropEvent) []domain.ID {
	var out []domain.ID
	if ev.Origin != "" && ev.Origin != "orchestrator" {
		out = append(out, ev.Origin)
	}
	for _, r := range ev.Observers {
		if !contains(out, r) {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

func hasRef(items []agents.ProposalItem, ref string) bool {
	for _, it := range items {
		if it.Ref == ref {
			return true
		}
	}
	return false
}

func contains(ids []domain.ID, id domain.ID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// affectedRegions derives the regions an event record should list: the
// origin plus every region whose state or entities the event touches.
// This is geographical relevance for filtering and inspection, not
// access: see observers.
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
	// pv adjudicates with full canonical history; viewPV validates a
	// regional agent's proposals with only that region's knowledge.
	// One agent's private information must never validate another's
	// reach, so the two must stay separate.
	pv     agents.ProposalValidator
	viewPV agents.ProposalValidator
	used   int
}

func (s *stepRun) askLogographer(ctx context.Context, regionID string, view *knowledge.LogographerView, add string) (*agents.LogographerReply, error) {
	prompt := agents.RenderLogographerPrompt(s.snap.Meta, s.snap.Meta.ClockStep, view)
	var reply *agents.LogographerReply
	err := retryParse(s, ctx, "logographer:"+regionID, agents.SystemLogographer(), prompt, add, 0.8, s.b.MaxOutputTokens,
		func(text string) error {
			r, err := agents.ParseLogographerReply(text)
			if err != nil {
				return err
			}
			if err := s.viewPV.ValidateLogographerProposals(regionID, r.Events); err != nil {
				return err
			}
			created := agents.CreatedEntities(s.snap, r.Events)
			for _, bl := range r.Beliefs {
				if bl.SubjectID != "" && s.snap.Entities[bl.SubjectID] == nil && !created[bl.SubjectID] {
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

func (s *stepRun) askFollowUp(ctx context.Context, regionID string, view *knowledge.LogographerView, section string) (*agents.LogographerReply, error) {
	prompt := agents.RenderLogographerPrompt(s.snap.Meta, s.snap.Meta.ClockStep, view)
	add := section +
		"\nThis is what you asked to consult or recall. Give your final answer now in the same JSON format; no further lookups are available this step.\n"
	var reply *agents.LogographerReply
	err := retryParse(s, ctx, "logographer:"+regionID+":consult", agents.SystemLogographer(), prompt, add, 0.8, s.b.MaxOutputTokens,
		func(text string) error {
			r, err := agents.ParseLogographerReply(text)
			if err != nil {
				return err
			}
			if err := s.viewPV.ValidateLogographerProposals(regionID, r.Events); err != nil {
				return err
			}
			created := agents.CreatedEntities(s.snap, r.Events)
			for _, bl := range r.Beliefs {
				if bl.SubjectID != "" && s.snap.Entities[bl.SubjectID] == nil && !created[bl.SubjectID] {
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

// askReconcile asks a region to rewrite its draft memory and beliefs
// against the adjudicated outcome. It needs budget; without it the
// caller keeps the previous memory.
func (s *stepRun) askReconcile(ctx context.Context, regionID string, view *knowledge.LogographerView, in agents.ReconcileInput, final []domain.PropEvent) (*agents.ReconcileReply, error) {
	if s.used >= s.b.MaxModelCallsPerStep {
		return nil, fmt.Errorf("no call budget left for reconciliation")
	}
	prompt := agents.RenderLogographerPrompt(s.snap.Meta, s.snap.Meta.ClockStep, view)
	created := agents.CreatedEntities(s.snap, final)
	var reply *agents.ReconcileReply
	err := retryParse(s, ctx, "logographer:"+regionID+":reconcile", agents.SystemLogographer(), prompt,
		agents.RenderReconcileRequest(in), 0.3, s.b.MaxOutputTokens,
		func(text string) error {
			r, err := agents.ParseReconcileReply(text)
			if err != nil {
				return err
			}
			for _, bl := range r.Beliefs {
				if bl.SubjectID != "" && s.snap.Entities[bl.SubjectID] == nil && !created[bl.SubjectID] {
					return fmt.Errorf("belief subject %q is not a known entity", bl.SubjectID)
				}
			}
			reply = r
			return nil
		})
	return reply, err
}

func (s *stepRun) askOrchestrator(ctx context.Context, prompt agents.Prompt, reactionsOK, finalOnly bool, items []agents.ProposalItem) (*agents.OrchestratorReply, error) {
	var reply *agents.OrchestratorReply
	maxTok := s.b.MaxOutputTokensOrchestr
	if maxTok <= 0 {
		maxTok = 2 * s.b.MaxOutputTokens
	}
	err := retryParse(s, ctx, "orchestrator", agents.SystemOrchestrator(), prompt, "", 0.3, maxTok,
		func(text string) error {
			r, err := agents.ParseOrchestratorReply(text)
			if err != nil {
				return err
			}
			// Validate the merged event set here so id mistakes go back
			// to the orchestrator as corrections, not as a dead attempt.
			if err := s.pv.ValidateFinalEvents(r.Events); err != nil {
				return err
			}
			for _, ds := range r.DirectiveStatus {
				if d := s.snap.Directives[ds.ID]; d == nil || d.Status == "withdrawn" {
					return fmt.Errorf("unknown directive %q in directive_status", ds.ID)
				}
			}
			for ref := range r.Dispositions {
				if !hasRef(items, ref) {
					return fmt.Errorf("disposition for %q: no such proposal this step (refs are p1, p2, ...)", ref)
				}
			}
			if finalOnly && len(r.Reactions) > 0 {
				return fmt.Errorf("this is the final adjudication call: the reaction budget is spent, so reaction requests are not accepted. Resolve now: emit the merged events, or pause with a reason")
			}
			if reactionsOK {
				for region, rx := range r.Reactions {
					if err := s.pv.ValidateReaction(domain.ID(region), rx, items); err != nil {
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

// maxDiagnosisBytes bounds correction feedback so a verbose validator
// message cannot itself exhaust the prompt budget.
const maxDiagnosisBytes = 1500

// retryParse calls the model and feeds structured failures back for a
// bounded number of correction rounds.
func retryParse(s *stepRun, ctx context.Context, role, system string, base agents.Prompt, add string, temp float64, maxTokens int, validate func(string) error) error {
	correction := ""
	var lastErr error
	for tries := 0; tries <= s.b.MaxCorrectionRetries; tries++ {
		// The byte ceiling is applied after every addition — late
		// extras and correction feedback included — by dropping
		// optional context only. Mandatory material that cannot fit is
		// an error before inference, never a trimmed rule.
		prompt, err := base.Fit(add+correction, s.b.MaxPromptBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", role, err)
		}
		text, truncated, err := s.call(ctx, role, system, prompt, temp, maxTokens)
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
		if len(diagnosis) > maxDiagnosisBytes {
			diagnosis = diagnosis[:maxDiagnosisBytes] + "..."
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
	// Diagnostics must survive shutdown: recording runs on a short
	// detached context, so a cancelled inference context (Ctrl-C during
	// a run) still leaves the final call in the trace instead of
	// replacing the real error with a storage failure.
	recCtx, recCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	recErr := s.e.DB.RecordModelCall(recCtx, s.attempt, role, s.e.ModelName, hex.EncodeToString(sum[:]),
		reqJSON, respJSON, resp.InputTokens, resp.OutputTokens, dur, errStr,
		store.CallSettings{Temperature: temp, MaxTokens: maxTokens, PromptBytes: len(system) + len(prompt), PromptBudget: s.b.MaxPromptBytes})
	recCancel()
	if recErr != nil {
		if callErr != nil {
			return "", false, callErr
		}
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
