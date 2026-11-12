// Package agents renders role prompts, parses strict JSON replies, and
// structurally validates proposals before the engine considers them.
package agents

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/knowledge"
	"github.com/dnym/herodotos/internal/store"
)

//go:embed prompts/logographer.system.md
var logographerSystem string

//go:embed prompts/orchestrator.system.md
var orchestratorSystem string

// SystemLogographer returns the regional historian role instructions.
func SystemLogographer() string { return logographerSystem }

// SystemOrchestrator returns the adjudication role instructions.
func SystemOrchestrator() string { return orchestratorSystem }

// MemoryUpdate is a regional agent's compact memory revision.
type MemoryUpdate struct {
	Summary     string   `json:"summary"`
	OpenThreads []string `json:"open_threads,omitempty"`
}

// BeliefRecord is one in-world belief a logographer records for their
// region. It describes what inhabitants think, not canonical truth.
type BeliefRecord struct {
	SubjectID string `json:"subject_id,omitempty"` // entity id, or empty for a general belief
	Modality  string `json:"modality"`             // known, uncertain, mistaken
	Statement string `json:"statement"`
}

// LogographerReply is a parsed regional agent reply.
type LogographerReply struct {
	Prose   string             `json:"prose"`
	Events  []domain.PropEvent `json:"events"`
	Memory  *MemoryUpdate      `json:"memory,omitempty"`
	Beliefs []BeliefRecord     `json:"beliefs,omitempty"`
	// Consult requests the full text of up to 3 cataloged reference
	// documents; the engine answers with one bounded follow-up call.
	Consult []string `json:"consult,omitempty"`
}

// OrchestratorReply is a parsed adjudication reply.
type OrchestratorReply struct {
	Notes           string             `json:"notes"`
	Events          []domain.PropEvent `json:"events"`
	DirectiveStatus []DirectiveStatus  `json:"directive_status,omitempty"`
	Reactions       map[string]string  `json:"reactions,omitempty"`
	Pause           *Pause             `json:"pause,omitempty"`
}

// DirectiveStatus updates one directive's status.
type DirectiveStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Pause halts the step: a hard directive cannot be satisfied as stated.
type Pause struct {
	Reason      string `json:"reason"`
	DirectiveID string `json:"directive_id,omitempty"`
}

// maxEventsPerAgent bounds one agent's output size.
const maxEventsPerAgent = 6

// maxChangesPerEvent and maxPatchKeys keep model payloads inside the
// output budget: terse events beat exhaustive ones.
const (
	maxChangesPerEvent = 8
	maxPatchKeys       = 10
)

// checkSize enforces the structural size limits common to all agents.
func checkSize(ev *domain.PropEvent, maxChanges, maxKeys int) error {
	if len(ev.Changes) > maxChanges {
		return fmt.Errorf("event %q has %d changes (max %d)", ev.Title, len(ev.Changes), maxChanges)
	}
	for _, ch := range ev.Changes {
		if len(ch.Patch) > maxKeys {
			return fmt.Errorf("change on %q has %d patch keys (max %d)", ch.ObjectID, len(ch.Patch), maxKeys)
		}
	}
	return nil
}

// ParseLogographerReply decodes a reply, tolerating code fences but
// requiring a complete JSON object.
func ParseLogographerReply(text string) (*LogographerReply, error) {
	var r LogographerReply
	if err := decodeStrictJSON(text, &r); err != nil {
		return nil, fmt.Errorf("invalid reply: %w", err)
	}
	if len(r.Events) > maxEventsPerAgent {
		return nil, fmt.Errorf("too many events: %d (max %d)", len(r.Events), maxEventsPerAgent)
	}
	for i := range r.Events {
		ev := &r.Events[i]
		if ev.Type == "" || ev.Title == "" || ev.Prose == "" {
			return nil, fmt.Errorf("event %d needs type, title, and prose", i)
		}
	}
	if err := validateBeliefs(r.Beliefs); err != nil {
		return nil, err
	}
	return &r, nil
}

// maxBeliefsPerAgent bounds how much in-world belief one reply may add.
const maxBeliefsPerAgent = 4

func validateBeliefs(bs []BeliefRecord) error {
	if len(bs) > maxBeliefsPerAgent {
		return fmt.Errorf("too many beliefs: %d (max %d)", len(bs), maxBeliefsPerAgent)
	}
	for i, bl := range bs {
		switch bl.Modality {
		case "known", "uncertain", "mistaken":
		default:
			return fmt.Errorf("belief %d: modality must be known, uncertain, or mistaken, got %q", i, bl.Modality)
		}
		if bl.Statement == "" || len(bl.Statement) > 400 {
			return fmt.Errorf("belief %d: statement must be present and at most 400 characters", i)
		}
		if bl.SubjectID != "" && !domain.ValidID(bl.SubjectID) {
			return fmt.Errorf("belief %d: invalid subject id %q", i, bl.SubjectID)
		}
	}
	return nil
}

// ParseOrchestratorReply decodes an adjudication reply.
func ParseOrchestratorReply(text string) (*OrchestratorReply, error) {
	var r OrchestratorReply
	if err := decodeStrictJSON(text, &r); err != nil {
		return nil, fmt.Errorf("invalid reply: %w", err)
	}
	if len(r.Events) > 3*maxEventsPerAgent {
		return nil, fmt.Errorf("too many events: %d", len(r.Events))
	}
	for i := range r.Events {
		ev := &r.Events[i]
		if ev.Type == "" || ev.Title == "" || ev.Prose == "" {
			return nil, fmt.Errorf("event %d needs type, title, and prose", i)
		}
	}
	for _, ds := range r.DirectiveStatus {
		switch ds.Status {
		case "pending", "active", "fulfilled", "conflicting":
		default:
			return nil, fmt.Errorf("directive %s: invalid status %q", ds.ID, ds.Status)
		}
	}
	return &r, nil
}

func decodeStrictJSON(text string, v any) error {
	s := strings.TrimSpace(text)
	// Strip markdown fences when models add them despite instructions.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimSpace(strings.TrimSuffix(strings.SplitN(s, "\n", 2)[1], "```"))
	}
	dec := json.NewDecoder(strings.NewReader(s))
	// Unknown keys usually mean the model invented a schema ("deliveries"
	// instead of "delivery"); silently dropping them would lose intent.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// RenderLogographerPrompt builds the step context for one regional agent.
func RenderLogographerPrompt(meta domain.Meta, currentStep int, v *knowledge.LogographerView, maxBytes int) string {
	var b strings.Builder
	date := stepDateLabel(meta, currentStep+1)
	fmt.Fprintf(&b, "WORLD: %s. Today (start of step %d): %s.\n\n", meta.WorldName, currentStep+1, date)
	fmt.Fprintf(&b, "YOUR REGION: id=%s name=%q status=%s\nconditions: %s\n",
		v.Region.ID, v.Region.Name, v.Region.Status, jsonCompact(v.Region.Attrs))
	if v.Memory != nil {
		fmt.Fprintf(&b, "YOUR MEMORY: %s\nopen threads: %s\n", v.Memory.Summary, jsonCompact(v.Memory.OpenThreads))
	}
	writeSection(&b, "ENTITIES IN YOUR REGION", func() {
		for _, e := range v.Entities {
			fmt.Fprintf(&b, "- %s [%s] %q status=%s attrs=%s\n", e.ID, e.Kind, e.Name, e.Status, jsonCompact(e.Attrs))
		}
	})
	writeSection(&b, "RELATIONSHIPS VISIBLE TO YOU", func() {
		for _, r := range v.Relationships {
			fmt.Fprintf(&b, "- %s: %s -%s-> %s (%s)\n", r.ID, r.SourceID, r.Kind, r.TargetID, r.Status)
		}
	})
	writeSection(&b, "YOUR CANONICAL RECENT EVENTS", func() {
		for _, ev := range v.RecentEvents {
			fmt.Fprintf(&b, "- %s step %d [%s] %s: %s\n", ev.ID, ev.Step, ev.Type, ev.Title, ev.Prose)
		}
	})
	writeSection(&b, "DELIVERED REPORTS (news from elsewhere; others decide their own actions)", func() {
		for _, r := range v.Reports {
			fmt.Fprintf(&b, "- %s (event step %d, delivered step %d): %s: %s\n", r.EventID, r.EventStep, r.DeliveredStep, r.Title, r.Prose)
		}
	})
	writeSection(&b, "IN-WORLD BELIEFS (may be wrong)", func() {
		for _, bl := range v.Beliefs {
			fmt.Fprintf(&b, "- %s about %s: %s\n", bl.Modality, orNone(bl.SubjectID), bl.Statement)
		}
	})
	writeSection(&b, "NEIGHBOURING REGIONS (identity only; you do not see their conditions)", func() {
		for _, id := range v.AdjacentIDs {
			fmt.Fprintf(&b, "- %s\n", id)
		}
	})
	writeSection(&b, "DIRECTIVES (creative direction, not fact)", func() {
		for _, d := range v.Directives {
			kind := "soft"
			if d.Hard {
				kind = "hard"
			}
			fmt.Fprintf(&b, "- %s [%s/%s]: %s\n", d.ID, kind, d.Status, d.Text)
		}
	})
	writeSection(&b, "REFERENCE DOCUMENTS (human-authored background)", func() {
		for _, ref := range v.Bindings {
			fmt.Fprintf(&b, "- %s (%s/BINDING): %s\n%s\n", ref.ID, ref.Scope, ref.Title, refBody(ref))
		}
		for _, ref := range v.Retrieved {
			fmt.Fprintf(&b, "- %s (%s/matched to current events): %s\n%s\n", ref.ID, ref.Scope, ref.Title, refBody(ref))
		}
		if len(v.Catalog) > 0 {
			b.WriteString("Catalog (listed for background awareness; full text not loaded this step):\n")
			for _, ref := range v.Catalog {
				fmt.Fprintf(&b, "- %s (%s): %s", ref.ID, ref.Scope, ref.Title)
				if ref.Description != "" {
					fmt.Fprintf(&b, " — %s", ref.Description)
				}
				b.WriteString("\n")
			}
		}
	})
	b.WriteString("\nAdvance this world one step as the historian of your region. Reply with the JSON object only.")
	out := b.String()
	if maxBytes > 0 && len(out) > maxBytes {
		out = out[:maxBytes] + "\n...[context truncated: prefer acting on what is shown]\n" +
			"Advance this world one step as the historian of your region. Reply with the JSON object only."
	}
	return out
}

// refBody renders a reference body with a display cap; the cap keeps a
// single oversized document from crowding out the rest of the prompt.
func refBody(ref domain.ReferenceDoc) string {
	body := strings.TrimSpace(ref.Body)
	if len(body) > 1200 {
		body = body[:1200] + "...[truncated]"
	}
	return body
}

// maxConsultDocs bounds one consult round: enough to ground a reply,
// small enough that the follow-up prompt stays inside its budget.
const maxConsultDocs = 3

// ConsultDocs resolves requested reference ids against the documents
// visible to the region. Unknown ids, duplicates, and everything past
// the cap are dropped rather than rejected: a bad guess costs nothing.
func ConsultDocs(requested []string, visible []domain.ReferenceDoc) []domain.ReferenceDoc {
	seen := map[string]bool{}
	var out []domain.ReferenceDoc
	for _, id := range requested {
		if len(out) >= maxConsultDocs {
			break
		}
		if seen[id] {
			continue
		}
		for _, doc := range visible {
			if string(doc.ID) == id {
				seen[id] = true
				out = append(out, doc)
				break
			}
		}
	}
	return out
}

// RenderConsultSection appends the full texts of consulted documents to
// a follow-up prompt. Requested bodies get a larger cap than automatic
// ones: the agent asked for exactly this text.
func RenderConsultSection(docs []domain.ReferenceDoc) string {
	var b strings.Builder
	writeSection(&b, "REFERENCE DOCUMENTS YOU REQUESTED (full text)", func() {
		for _, ref := range docs {
			body := strings.TrimSpace(ref.Body)
			if len(body) > 4000 {
				body = body[:4000] + "...[truncated]"
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n%s\n", ref.ID, ref.Scope, ref.Title, body)
		}
	})
	return b.String()
}

// RenderOrchestratorPrompt builds the adjudication context: everything,
// clearly separated into proposals, interactions, and canonical state.
func RenderOrchestratorPrompt(meta domain.Meta, currentStep int, snap *store.Snapshot,
	proposals map[string][]domain.PropEvent, interactions []Interaction, maxBytes int) string {
	var b strings.Builder
	date := stepDateLabel(meta, currentStep+1)
	fmt.Fprintf(&b, "WORLD: %s. Step about to begin: %d (%s). Current clock: %d.\n\n",
		meta.WorldName, currentStep+1, date, currentStep)

	writeSection(&b, "REGIONS", func() {
		for id, r := range snap.Regions {
			fmt.Fprintf(&b, "- %s %q [%s] %s\n", id, r.Name, r.Status, jsonCompact(r.Attrs))
		}
	})
	writeSection(&b, "CONNECTIONS", func() {
		for _, c := range snap.Connections {
			fmt.Fprintf(&b, "- %s <-> %s (%d days)\n", c.A, c.B, c.TravelDay)
		}
	})
	writeSection(&b, "DIRECTIVES", func() {
		for _, d := range snap.Directives {
			kind := "soft"
			if d.Hard {
				kind = "hard"
			}
			fmt.Fprintf(&b, "- %s [%s/%s]: %s\n", d.ID, kind, d.Status, d.Text)
		}
	})
	writeSection(&b, "BINDING REFERENCES (world rules: adjudicate against these)", func() {
		for _, ref := range snap.WorldRefs {
			if !ref.Binding {
				continue
			}
			fmt.Fprintf(&b, "- %s (world): %s\n%s\n", ref.ID, ref.Title, refBody(ref))
		}
		var regionIDs []string
		for id := range snap.RegionRefs {
			regionIDs = append(regionIDs, id)
		}
		sort.Strings(regionIDs)
		for _, id := range regionIDs {
			for _, ref := range snap.RegionRefs[id] {
				if !ref.Binding {
					continue
				}
				fmt.Fprintf(&b, "- %s (region %s): %s\n%s\n", ref.ID, id, ref.Title, refBody(ref))
			}
		}
	})
	writeSection(&b, "RECENT CANONICAL EVENTS", func() {
		for _, ev := range snap.RecentEvents {
			fmt.Fprintf(&b, "- %s step %d [%s] %s: %s\n", ev.ID, ev.Step, ev.Type, ev.Title, ev.Prose)
		}
	})
	writeSection(&b, "PROPOSALS (independent; nothing here is committed)", func() {
		for author, evs := range proposals {
			for i, ev := range evs {
				fmt.Fprintf(&b, "- [%s #%d] %s: %s | causes=%s changes=%s entities=%s delivery=%s\n",
					author, i, ev.Title, ev.Prose, jsonCompact(ev.Causes), jsonCompact(ev.Changes),
					jsonCompact(ev.AffectsEntities), jsonCompact(ev.Delivery))
			}
		}
	})
	writeSection(&b, "DETECTED INTERACTIONS (structural; semantic conflicts may exist beyond this list)", func() {
		for _, in := range interactions {
			fmt.Fprintf(&b, "- %s: %s among %s\n", in.Kind, strings.Join(in.Objects, ", "), strings.Join(in.Authors, ", "))
		}
	})
	b.WriteString("\nProduce the single shared history for this step. Reply with the JSON object only.")
	out := b.String()
	if maxBytes > 0 && len(out) > maxBytes {
		out = out[:maxBytes] + "\n...[context truncated]\nProduce the single shared history for this step. Reply with the JSON object only."
	}
	return out
}

func writeSection(b *strings.Builder, title string, fn func()) {
	b.WriteString(title + ":\n")
	fn()
	b.WriteString("\n")
}

// stepDateLabel renders the display calendar date at a step boundary.
func stepDateLabel(meta domain.Meta, step int) string {
	t, err := meta.StepDate(step)
	if err != nil {
		return fmt.Sprintf("step %d", step)
	}
	return t.Format("2 Jan 2006")
}

func orNone(s string) string {
	if s == "" {
		return "the region"
	}
	return s
}

func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// --- validation ---

// ProposalValidator checks structural rules that ordinary code can
// enforce: identities, region authority, and delivery times.
type ProposalValidator struct {
	Snap        *store.Snapshot
	CurrentStep int // committed clock; commitment will produce CurrentStep+1
}

// ValidateLogographerProposal enforces regional authority (FR-021): an
// agent may change only its own region and entities, and may not touch
// directives.
func (pv *ProposalValidator) ValidateLogographerProposal(authorRegion domain.ID, ev *domain.PropEvent) error {
	newStep := pv.CurrentStep + 1
	if len(ev.Title) > 200 || len(ev.Prose) > 4000 {
		return fmt.Errorf("event text too long")
	}
	if err := checkSize(ev, maxChangesPerEvent, maxPatchKeys); err != nil {
		return err
	}
	if ev.Origin != "" && ev.Origin != authorRegion {
		return fmt.Errorf("origin %q is not your region", ev.Origin)
	}
	for _, ch := range ev.Changes {
		if !domain.ValidID(ch.ObjectID) {
			return fmt.Errorf("invalid object id %q", ch.ObjectID)
		}
		switch ch.Object {
		case "region":
			if ch.ObjectID != authorRegion {
				return fmt.Errorf("region authority: you may not change %q (FR-021)", ch.ObjectID)
			}
		case "entity":
			e := pv.Snap.Entities[ch.ObjectID]
			if e == nil {
				if ch.Op != "upsert" {
					return fmt.Errorf("entity %q does not exist", ch.ObjectID)
				}
				if rid, _ := ch.Patch["region_id"].(string); rid != "" && rid != authorRegion {
					return fmt.Errorf("entity upsert %q must be located in your region", ch.ObjectID)
				}
			} else if e.RegionID != authorRegion {
				return fmt.Errorf("entity %q is not yours to change", ch.ObjectID)
			}
		case "relationship":
			r := pv.Snap.Relationships[ch.ObjectID]
			if r == nil && ch.Op != "upsert" {
				return fmt.Errorf("relationship %q does not exist", ch.ObjectID)
			}
		case "directive":
			return fmt.Errorf("directives may not be changed by regional agents")
		default:
			return fmt.Errorf("unknown object kind %q", ch.Object)
		}
		switch ch.Op {
		case "set_attrs", "upsert", "set_status":
		default:
			return fmt.Errorf("invalid op %q", ch.Op)
		}
	}
	for _, id := range ev.AffectsEntities {
		if _, ok := pv.Snap.Entities[id]; !ok {
			return fmt.Errorf("unknown entity %q", id)
		}
	}
	for _, id := range ev.Causes {
		if !knownCause(pv.Snap, id) {
			return fmt.Errorf("unknown cause %q", id)
		}
	}
	for region, step := range ev.Delivery {
		if _, ok := pv.Snap.Regions[region]; !ok {
			return fmt.Errorf("delivery to unknown region %q", region)
		}
		if region == authorRegion {
			return fmt.Errorf("delivery to your own region is meaningless")
		}
		if step <= newStep {
			return fmt.Errorf("delivery step %d must be greater than %d", step, newStep)
		}
	}
	return nil
}

// ValidateFinalEvent checks orchestrator output structurally. Cross-region
// changes are allowed for the orchestrator, but every id must resolve and
// deliveries must respect the clock.
func (pv *ProposalValidator) ValidateFinalEvent(ev *domain.PropEvent) error {
	newStep := pv.CurrentStep + 1
	// Merged outcomes may carry more changes than a single agent's event.
	if err := checkSize(ev, 2*maxChangesPerEvent, 2*maxPatchKeys); err != nil {
		return err
	}
	if ev.Origin != "" && ev.Origin != "orchestrator" {
		if _, ok := pv.Snap.Regions[ev.Origin]; !ok {
			return fmt.Errorf("origin %q is not a region", ev.Origin)
		}
	}
	for _, ch := range ev.Changes {
		if !domain.ValidID(ch.ObjectID) {
			return fmt.Errorf("invalid object id %q", ch.ObjectID)
		}
		switch ch.Object {
		case "region":
			if pv.Snap.Regions[ch.ObjectID] == nil && ch.Op != "upsert" {
				return fmt.Errorf("region %q does not exist", ch.ObjectID)
			}
		case "entity":
			if pv.Snap.Entities[ch.ObjectID] == nil && ch.Op != "upsert" {
				return fmt.Errorf("entity %q does not exist", ch.ObjectID)
			}
		case "relationship":
			if pv.Snap.Relationships[ch.ObjectID] == nil && ch.Op != "upsert" {
				return fmt.Errorf("relationship %q does not exist", ch.ObjectID)
			}
		case "directive":
			if pv.Snap.Directives[ch.ObjectID] == nil {
				return fmt.Errorf("directive %q does not exist", ch.ObjectID)
			}
		default:
			return fmt.Errorf("unknown object kind %q", ch.Object)
		}
		switch ch.Op {
		case "set_attrs", "upsert", "set_status":
		default:
			return fmt.Errorf("invalid op %q", ch.Op)
		}
	}
	for _, id := range ev.AffectsEntities {
		if _, ok := pv.Snap.Entities[id]; !ok {
			return fmt.Errorf("unknown entity %q", id)
		}
	}
	for _, id := range ev.Causes {
		if !knownCause(pv.Snap, id) {
			return fmt.Errorf("unknown cause %q", id)
		}
	}
	// Once canonical history is visible, every orchestrator event must
	// be placed on the causal graph: it follows from something shown.
	if len(pv.Snap.RecentEvents) > 0 && len(ev.Causes) == 0 {
		return fmt.Errorf("event %q cites no causes: name at least one visible canonical event id in \"causes\" that it follows from", ev.Title)
	}
	for region, step := range ev.Delivery {
		if _, ok := pv.Snap.Regions[region]; !ok {
			return fmt.Errorf("delivery to unknown region %q", region)
		}
		if step <= newStep {
			return fmt.Errorf("delivery step %d must be greater than %d", step, newStep)
		}
	}
	return nil
}

func knownCause(s *store.Snapshot, id domain.ID) bool {
	for _, ev := range s.RecentEvents {
		if ev.ID == id {
			return true
		}
	}
	return false
}

// --- interaction detection ---

// Interaction describes proposals whose outcomes cannot be decided
// independently.
type Interaction struct {
	Kind    string // shared_write, shared_entity, cross_border_action
	Objects []string
	Authors []string
}

// DetectInteractions performs the structural pass: overlapping write
// targets, shared participants, and actions landing in another region's
// territory. The orchestrator reviews semantic conflicts itself.
func DetectInteractions(proposals map[string][]domain.PropEvent) []Interaction {
	type key struct{ kind, object string }
	writes := map[key][]string{}
	participants := map[domain.ID][]string{}
	entering := map[string]map[string]string{} // region -> entity id -> author

	for author, evs := range proposals {
		for _, ev := range evs {
			for _, ch := range ev.Changes {
				k := key{ch.Object, ch.ObjectID}
				writes[k] = append(writes[k], author)
				if ch.Object == "entity" {
					participants[ch.ObjectID] = append(participants[ch.ObjectID], author)
					if rid, _ := ch.Patch["region_id"].(string); rid != "" {
						if !strings.HasPrefix(author, "logographer:") || strings.TrimPrefix(author, "logographer:") != rid {
							if entering[rid] == nil {
								entering[rid] = map[string]string{}
							}
							entering[rid][ch.ObjectID] = author
						}
					}
				}
			}
			for _, id := range ev.AffectsEntities {
				participants[id] = append(participants[id], author)
			}
		}
	}

	var out []Interaction
	seen := map[string]bool{}
	add := func(kind string, objs, authors []string) {
		sig := kind + "|" + strings.Join(objs, ",") + "|" + strings.Join(authors, ",")
		if seen[sig] {
			return
		}
		seen[sig] = true
		out = append(out, Interaction{Kind: kind, Objects: objs, Authors: authors})
	}
	for k, authors := range writes {
		if len(authors) > 1 {
			add("shared_write", []string{k.kind + ":" + k.object}, dedupe(authors))
		}
	}
	for id, authors := range participants {
		if len(dedupe(authors)) > 1 {
			add("shared_entity", []string{id}, dedupe(authors))
		}
	}
	for region, ents := range entering {
		var authors []string
		var objs []string
		for obj, author := range ents {
			objs = append(objs, obj)
			authors = append(authors, author)
		}
		local := "logographer:" + region
		for a := range proposals {
			if a != local {
				continue
			}
			if len(objs) > 0 {
				add("cross_border_action", objs, dedupe(append(authors, local)))
			}
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
