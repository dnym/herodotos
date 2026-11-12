// Package sim owns the step lifecycle: snapshot, independent proposals,
// interaction detection, adjudication, and atomic commitment. Agents
// propose; only this package commits.
package sim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		author := "logographer:" + regionID
		if len(reply.Events) > 0 {
			proposals[author] = append(proposals[author], reply.Events...)
		}
		if reply.Memory != nil && reply.Memory.Summary != "" {
			memories[regionID] = reply.Memory
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
		reply, err := st.askOrchestrator(ctx, prompt)
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
			question := reply.Reactions[regionID]
			reply2, err := st.askLogographer(ctx, regionID, view, question)
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
	for _, ev := range final.Events {
		if err := st.pv.ValidateFinalEvent(&ev); err != nil {
			return fail("final event rejected", err)
		}
		if ev.Origin == "" {
			ev.Origin = "orchestrator"
		}
		ce := store.CommittedEvent{
			Type: ev.Type, Title: ev.Title, Prose: ev.Prose,
			Changes:   ev.Changes,
			EntityIDs: ev.AffectsEntities,
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
			Prose: "The orchestrator reviewed standing directives.",
			Changes: changes,
		})
	}

	for regionID, m := range memories {
		plan.Memories = append(plan.Memories, domain.Memory{
			RegionID: regionID, Summary: m.Summary, OpenThreads: m.OpenThreads, UpdatedStep: newStep,
		})
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

func (s *stepRun) askLogographer(ctx context.Context, regionID string, view *knowledge.LogographerView, extra string) (*agents.LogographerReply, error) {
	prompt := agents.RenderLogographerPrompt(s.snap.Meta, s.snap.Meta.ClockStep, view, s.b.MaxPromptBytes)
	if extra != "" {
		prompt += "\nTHE ORCHESTRATOR ASKS YOU: " + extra + "\nAnswer with the same JSON format, reacting only within your authority.\n"
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
			reply = r
			return nil
		})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

func (s *stepRun) askOrchestrator(ctx context.Context, prompt string) (*agents.OrchestratorReply, error) {
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
		text, err := s.call(ctx, role, system, prompt+correction, temp, maxTokens)
		if err != nil {
			return err
		}
		lastErr = validate(text)
		if lastErr == nil {
			return nil
		}
		correction = "\n\nYour previous reply was rejected: " + lastErr.Error() + "\nReply with only a corrected JSON object."
	}
	return fmt.Errorf("exhausted %d correction retries (last: %v)", s.b.MaxCorrectionRetries, lastErr)
}

// call performs one model interaction inside the per-step budget.
// maxTokens <= 0 falls back to the logographer ceiling.
func (s *stepRun) call(ctx context.Context, role, system, prompt string, temp float64, maxTokens int) (string, error) {
	s.used++
	if s.used > s.b.MaxModelCallsPerStep {
		return "", fmt.Errorf("model call budget (%d) exhausted", s.b.MaxModelCallsPerStep)
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
	if callErr == nil {
		respJSON = jsonPreview(map[string]string{"text": resp.Text})
	}
	errStr := ""
	if callErr != nil {
		errStr = callErr.Error()
	}
	if recErr := s.e.DB.RecordModelCall(ctx, s.attempt, role, s.e.ModelName, hex.EncodeToString(sum[:]),
		reqJSON, respJSON, resp.InputTokens, resp.OutputTokens, dur, errStr); recErr != nil {
		return "", fmt.Errorf("record model call: %w", recErr)
	}
	if callErr != nil {
		return "", fmt.Errorf("inference call for %s: %w", role, callErr)
	}
	return resp.Text, nil
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
