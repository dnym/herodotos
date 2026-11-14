// Package agents renders role prompts, parses strict JSON replies, and
// structurally validates proposals before the engine considers them.
package agents

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
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
	// Recall looks up the region's own older history: up to 3 event
	// ids or keyword queries, answered in the same follow-up call from
	// what the region witnessed or was told.
	Recall []string `json:"recall,omitempty"`
}

// ReactionRequest is one request for a regional reaction. The premise
// names the canonical events the question rests on, so the engine can
// verify the responding region could actually know them: a reaction
// must not become a side channel for not-yet-delivered news.
type ReactionRequest struct {
	Question string      `json:"question"`
	Premise  []domain.ID `json:"premise"`
}

// OrchestratorReply is a parsed adjudication reply.
type OrchestratorReply struct {
	Notes           string                     `json:"notes"`
	Events          []domain.PropEvent         `json:"events"`
	DirectiveStatus []DirectiveStatus          `json:"directive_status,omitempty"`
	Reactions       map[string]ReactionRequest `json:"reactions,omitempty"`
	Pause           *Pause                     `json:"pause,omitempty"`
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
	if len(r.Recall) > 3 {
		return nil, fmt.Errorf("too many recall queries: %d (max 3)", len(r.Recall))
	}
	for _, q := range r.Recall {
		if strings.TrimSpace(q) == "" || len(q) > 100 {
			return nil, fmt.Errorf("recall queries must be non-empty and at most 100 characters")
		}
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
		if strings.Contains(err.Error(), "reactions") {
			return nil, fmt.Errorf("invalid reply: %w; reactions must be {\"region id\": {\"question\": \"...\", \"premise\": [\"evt-...\"]}} naming canonical events the region can already know", err)
		}
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
	for region, rx := range r.Reactions {
		if strings.TrimSpace(rx.Question) == "" {
			return nil, fmt.Errorf("reaction for %s: question must not be empty", region)
		}
		for _, id := range rx.Premise {
			if !domain.ValidID(id) {
				return nil, fmt.Errorf("reaction for %s: invalid premise id %q", region, id)
			}
		}
	}
	return &r, nil
}

func decodeStrictJSON(text string, v any) error {
	s := strings.TrimSpace(text)
	// Strip markdown fences when models add them despite instructions —
	// defensively: a lone "```" from a cut-off reply must produce an
	// error that feeds the correction round, never a panic.
	if strings.HasPrefix(s, "```") {
		_, tail, ok := strings.Cut(s, "\n")
		if !ok {
			return fmt.Errorf("reply is a stray code fence with no JSON")
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(tail), "```"))
	}
	if !strings.HasPrefix(s, "{") {
		return fmt.Errorf("reply is not a JSON object (starts with %q)", firstRune(s))
	}
	dec := json.NewDecoder(strings.NewReader(s))
	// Unknown keys usually mean the model invented a schema ("deliveries"
	// instead of "delivery"); silently dropping them would lose intent.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Exactly one JSON value: a second object, trailing prose, or a stray
	// closing delimiter means the model answered twice, narrated after
	// the JSON, or lost track of its nesting. Decoder.More() is not an
	// end-of-input check (it reports false before "]" or "}"), so a
	// second decode must hit io.EOF.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("reply carries trailing content after the JSON object")
	}
	return nil
}

func firstRune(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)[0]
	if r > 127 {
		return "?"
	}
	return string(r)
}

// Prompt sections carry an explicit priority. Mandatory sections — the
// user's creative direction, the world's binding rules, and the
// decision material a role cannot act correctly without — are never
// dropped for space. Optional sections are included in priority order
// until the budget runs out, lowest priority out first. If the
// mandatory material plus the immediate task cannot fit, assembly fails
// with a BudgetError before any inference call: a silently amnesiac
// agent is worse than a loudly broken budget.
type promptSection struct {
	heading   string
	body      string
	mandatory bool
}

func (ps promptSection) full() string { return ps.heading + "\n" + ps.body }

// Prompt is a rendered prompt kept structured until the final
// assembly, so late additions (a reaction request, consulted texts,
// correction feedback) can be fitted without ever trimming a mandatory
// section to make room.
type Prompt struct {
	head     string
	footer   string
	sections []promptSection
}

// BudgetError reports that a prompt's required material cannot fit the
// configured byte ceiling. It is an application error: the engine must
// stop before inference rather than send a prompt missing binding
// rules or the task itself.
type BudgetError struct {
	Required int // bytes of head, mandatory sections, additions, and footer
	Max      int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("prompt budget: mandatory material and the immediate task need %d bytes but the ceiling is %d; raise HERODOTOS_BUDGET_MAX_PROMPT_BYTES or shorten directives and binding references", e.Required, e.Max)
}

func (p Prompt) assemble(include []bool, extra string) string {
	var b strings.Builder
	b.WriteString(p.head)
	b.WriteString("\n")
	for i, sec := range p.sections {
		if include != nil && !include[i] {
			continue
		}
		b.WriteString(sec.full())
		b.WriteString("\n")
	}
	b.WriteString(p.footer)
	b.WriteString(extra)
	return b.String()
}

// String renders the whole prompt with no byte ceiling.
func (p Prompt) String() string { return p.assemble(nil, "") }

// Fit assembles the prompt with late additions under maxBytes (<= 0
// means unbounded). Optional sections are dropped lowest priority
// first; mandatory sections and the additions are never cut. When even
// those cannot fit, Fit returns a *BudgetError and no prompt.
func (p Prompt) Fit(extra string, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		return p.assemble(nil, extra), nil
	}
	include := make([]bool, len(p.sections))
	for i := range include {
		include[i] = true
	}
	out := p.assemble(include, extra)
	for i := len(p.sections) - 1; i >= 0 && len(out) > maxBytes; i-- {
		if p.sections[i].mandatory {
			continue
		}
		include[i] = false
		out = p.assemble(include, extra)
	}
	if len(out) > maxBytes {
		return "", &BudgetError{Required: len(out), Max: maxBytes}
	}
	return out, nil
}

// logographerFooter is the instruction closing every regional prompt.
const logographerFooter = "Advance this world one step as the historian of your region. Reply with the JSON object only."

// RenderLogographerPrompt builds the step context for one regional agent.
func RenderLogographerPrompt(meta domain.Meta, currentStep int, v *knowledge.LogographerView) Prompt {
	var hb strings.Builder
	date := stepDateLabel(meta, currentStep+1)
	fmt.Fprintf(&hb, "WORLD: %s. Today (start of step %d): %s.\n\n", meta.WorldName, currentStep+1, date)
	fmt.Fprintf(&hb, "YOUR REGION: id=%s name=%q status=%s\nconditions: %s\n",
		v.Region.ID, v.Region.Name, v.Region.Status, jsonCompact(v.Region.Attrs))
	if v.Memory != nil {
		fmt.Fprintf(&hb, "YOUR MEMORY: %s\nopen threads: %s\n", v.Memory.Summary, jsonCompact(v.Memory.OpenThreads))
	}

	var bindings strings.Builder
	for _, ref := range v.Bindings {
		fmt.Fprintf(&bindings, "- %s (%s/BINDING): %s\n%s\n", ref.ID, ref.Scope, ref.Title, refFullText(ref))
	}
	var directives strings.Builder
	for _, d := range v.Directives {
		kind := "soft"
		if d.Hard {
			kind = "hard"
		}
		fmt.Fprintf(&directives, "- %s [%s/%s]: %s\n", d.ID, kind, d.Status, d.Text)
	}
	var entities strings.Builder
	for _, e := range v.Entities {
		fmt.Fprintf(&entities, "- %s [%s] %q status=%s attrs=%s\n", e.ID, e.Kind, e.Name, e.Status, jsonCompact(e.Attrs))
	}
	var relationships strings.Builder
	for _, r := range v.Relationships {
		fmt.Fprintf(&relationships, "- %s: %s -%s-> %s (%s)\n", r.ID, r.SourceID, r.Kind, r.TargetID, r.Status)
	}
	var recent strings.Builder
	for _, ev := range v.RecentEvents {
		fmt.Fprintf(&recent, "- %s step %d [%s] %s: %s\n", ev.ID, ev.Step, ev.Type, ev.Title, ev.Prose)
	}
	reportHeading := "DELIVERED REPORTS (news from elsewhere; others decide their own actions)"
	if v.ReportsHidden > 0 {
		reportHeading = fmt.Sprintf("DELIVERED REPORTS (newest first, %d older reports omitted; their events remain known history you may cite)", v.ReportsHidden)
	}
	var reports strings.Builder
	for _, r := range v.Reports {
		fmt.Fprintf(&reports, "- %s (event step %d, delivered step %d): %s: %s\n", r.EventID, r.EventStep, r.DeliveredStep, r.Title, r.Prose)
	}
	var beliefs strings.Builder
	for _, bl := range v.Beliefs {
		fmt.Fprintf(&beliefs, "- %s about %s: %s\n", bl.Modality, orNone(bl.SubjectID), bl.Statement)
	}
	var neighbors strings.Builder
	for _, tn := range v.Travel {
		kind := "via other regions"
		if tn.Neighbour {
			kind = "neighbour"
		}
		fmt.Fprintf(&neighbors, "- %s (%s, %d days): news of this step's events arrives at step %d at the earliest\n",
			tn.RegionID, kind, tn.Days, tn.EarliestStep)
	}
	var refs strings.Builder
	for _, ref := range v.Retrieved {
		fmt.Fprintf(&refs, "- %s (%s/matched to current events): %s\n%s\n", ref.ID, ref.Scope, ref.Title, refBody(ref))
	}
	if len(v.Catalog) > 0 {
		refs.WriteString("Catalog (listed for background awareness; full text not loaded this step):\n")
		for _, ref := range v.Catalog {
			fmt.Fprintf(&refs, "- %s (%s): %s", ref.ID, ref.Scope, ref.Title)
			if ref.Description != "" {
				fmt.Fprintf(&refs, " — %s", ref.Description)
			}
			refs.WriteString("\n")
		}
	}

	return Prompt{head: hb.String(), footer: "\n" + logographerFooter, sections: []promptSection{
		// Mandatory: creative direction and world rules survive any
		// budget pressure.
		{heading: "DIRECTIVES (creative direction, not fact)", body: directives.String(), mandatory: true},
		{heading: "REFERENCE DOCUMENTS — BINDING (world rules: act within these)", body: bindings.String(), mandatory: true},
		// Optional, most important first: what is happening now, then
		// supporting background, then the reference catalogue.
		{heading: "RECENT EVENTS YOUR REGION WITNESSED (canonical, as your region knows them)", body: recent.String()},
		{heading: reportHeading, body: reports.String()},
		{heading: "ENTITIES IN YOUR REGION", body: entities.String()},
		{heading: "RELATIONSHIPS VISIBLE TO YOU", body: relationships.String()},
		{heading: "IN-WORLD BELIEFS (may be wrong)", body: beliefs.String()},
		{heading: fmt.Sprintf("NEWS TRAVEL (one step is %d days; regions listed by identity only, you do not see their conditions; unlisted regions have no known route)", v.StepDays), body: neighbors.String()},
		{heading: "REFERENCE DOCUMENTS (advisory, matched to current events)", body: refs.String()},
	}}
}

// refBody renders an advisory reference body with a display cap: a
// single large document must not crowd out current events.
func refBody(ref domain.ReferenceDoc) string {
	body := strings.TrimSpace(ref.Body)
	if len(body) > 1200 {
		body = body[:1200] + "...[truncated]"
	}
	return body
}

// refFullText renders a binding reference without an extra cap:
// binding rules are mandatory prompt material, protected by section
// budgeting rather than silently cut.
func refFullText(ref domain.ReferenceDoc) string {
	return strings.TrimSpace(ref.Body)
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
	b.WriteString("\n\n")
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

// RenderRecallSection lists the region's own history entries a recall
// query found, in the version the region knows.
func RenderRecallSection(queries []string, events []domain.Event) string {
	var b strings.Builder
	b.WriteString("\n\n")
	writeSection(&b, "YOUR HISTORY LOOKUP (queries: "+strings.Join(queries, "; ")+")", func() {
		if len(events) == 0 {
			b.WriteString("- nothing in your region's knowledge matches\n")
		}
		for _, ev := range events {
			fmt.Fprintf(&b, "- %s step %d [%s] %s: %s\n", ev.ID, ev.Step, ev.Type, ev.Title, ev.Prose)
		}
	})
	return b.String()
}

// orchestratorFooter closes every adjudication prompt.
const orchestratorFooter = "Produce the single shared history for this step. Reply with the JSON object only."

// ProposalItem is one proposed event with its attempt-stable reference
// ("p1", "p2", ...). The orchestrator reports a disposition per
// reference; the engine compares proposals with final outcomes by it.
type ProposalItem struct {
	Ref    string
	Author string // logographer:<region-id>
	Event  domain.PropEvent
}

// ProposalMap groups items by author, the shape interaction detection
// works on.
func ProposalMap(items []ProposalItem) map[string][]domain.PropEvent {
	out := map[string][]domain.PropEvent{}
	for _, it := range items {
		out[it.Author] = append(out[it.Author], it.Event)
	}
	return out
}

// RenderOrchestratorPrompt builds the adjudication context: everything,
// clearly separated into proposals, interactions, and canonical state.
// Directives, binding references, the proposals themselves, and the
// canonical records the proposals touch are mandatory — adjudicating an
// action without the facts needed to validate it is not adjudication.
// Everything else is included under the byte ceiling in priority order.
func RenderOrchestratorPrompt(meta domain.Meta, currentStep int, snap *store.Snapshot,
	items []ProposalItem, interactions []Interaction) Prompt {
	var hb strings.Builder
	date := stepDateLabel(meta, currentStep+1)
	fmt.Fprintf(&hb, "WORLD: %s. Step about to begin: %d (%s). Current clock: %d. One step lasts %d days.\n\n",
		meta.WorldName, currentStep+1, date, currentStep, meta.StepDays)

	// The canonical records proposals touch are decision state.
	touched := map[domain.ID]bool{}
	touchedRel := map[domain.ID]bool{}
	for _, it := range items {
		for _, ch := range it.Event.Changes {
			switch ch.Object {
			case "entity":
				touched[ch.ObjectID] = true
			case "relationship":
				touchedRel[ch.ObjectID] = true
				for _, k := range []string{"source", "target"} {
					if id, _ := ch.Patch[k].(string); id != "" {
						touched[id] = true
					}
				}
			}
		}
		for _, id := range it.Event.AffectsEntities {
			touched[id] = true
		}
	}

	var regions strings.Builder
	for _, id := range sortedIDs(mapKeys(snap.Regions)) {
		r := snap.Regions[id]
		fmt.Fprintf(&regions, "- %s %q [%s] %s\n", id, r.Name, r.Status, jsonCompact(r.Attrs))
	}
	var connections strings.Builder
	for _, c := range snap.Connections {
		fmt.Fprintf(&connections, "- %s <-> %s (%d days)\n", c.A, c.B, c.TravelDay)
	}
	var touchedEntities, otherEntities strings.Builder
	for _, id := range sortedIDs(mapKeys(snap.Entities)) {
		e := snap.Entities[id]
		sb := &otherEntities
		if touched[id] {
			sb = &touchedEntities
		}
		fmt.Fprintf(sb, "- %s %q kind=%s region=%s status=%s %s\n",
			id, e.Name, e.Kind, e.RegionID, e.Status, jsonCompact(e.Attrs))
	}
	var touchedRels, otherRels strings.Builder
	for _, id := range sortedIDs(mapKeys(snap.Relationships)) {
		r := snap.Relationships[id]
		sb := &otherRels
		if touchedRel[id] || touched[r.SourceID] || touched[r.TargetID] {
			sb = &touchedRels
		}
		fmt.Fprintf(sb, "- %s %s -> %s kind=%s status=%s %s\n",
			r.ID, r.SourceID, r.TargetID, r.Kind, r.Status, jsonCompact(r.Attrs))
	}
	var directives strings.Builder
	for _, id := range sortedIDs(mapKeys(snap.Directives)) {
		d := snap.Directives[id]
		kind := "soft"
		if d.Hard {
			kind = "hard"
		}
		fmt.Fprintf(&directives, "- %s [%s/%s]: %s\n", d.ID, kind, d.Status, d.Text)
	}
	var bindings strings.Builder
	for _, ref := range snap.WorldRefs {
		if !ref.Binding {
			continue
		}
		fmt.Fprintf(&bindings, "- %s (world): %s\n%s\n", ref.ID, ref.Title, refFullText(ref))
	}
	for _, id := range sortedStrings(mapKeys(snap.RegionRefs)) {
		for _, ref := range snap.RegionRefs[domain.ID(id)] {
			if !ref.Binding {
				continue
			}
			fmt.Fprintf(&bindings, "- %s (region %s): %s\n%s\n", ref.ID, id, ref.Title, refFullText(ref))
		}
	}
	var recent strings.Builder
	for _, ev := range snap.RecentEvents {
		fmt.Fprintf(&recent, "- %s step %d [%s] %s: %s\n", ev.ID, ev.Step, ev.Type, ev.Title, ev.Prose)
	}
	var proposalsSb strings.Builder
	for _, it := range items {
		ev := it.Event
		fmt.Fprintf(&proposalsSb, "- %s [%s] %s: %s | causes=%s changes=%s entities=%s observers=%s accounts=%s delivery=%s expedited=%s\n",
			it.Ref, it.Author, ev.Title, ev.Prose, jsonCompact(ev.Causes), jsonCompact(ev.Changes),
			jsonCompact(ev.AffectsEntities), jsonCompact(ev.Observers), jsonCompact(ev.Accounts), jsonCompact(ev.Delivery), jsonCompact(ev.Expedited))
	}
	var interactionsSb strings.Builder
	for _, in := range interactions {
		fmt.Fprintf(&interactionsSb, "- %s: %s among %s\n", in.Kind, strings.Join(in.Objects, ", "), strings.Join(in.Authors, ", "))
	}

	return Prompt{head: hb.String(), footer: "\n" + orchestratorFooter, sections: []promptSection{
		// Mandatory: the user's direction, the world's rules, and the
		// decision material — proposals plus the canonical records they
		// touch — are what adjudication is against.
		{heading: "DIRECTIVES", body: directives.String(), mandatory: true},
		{heading: "BINDING REFERENCES (world rules: adjudicate against these)", body: bindings.String(), mandatory: true},
		{heading: "PROPOSALS (independent; nothing here is committed)", body: proposalsSb.String(), mandatory: true},
		{heading: "DETECTED INTERACTIONS (structural; semantic conflicts may exist beyond this list)", body: interactionsSb.String(), mandatory: true},
		{heading: "ENTITIES TOUCHED BY PROPOSALS (canonical; proposals may contradict them — you adjudicate)", body: touchedEntities.String(), mandatory: true},
		{heading: "RELATIONSHIPS TOUCHED BY PROPOSALS (canonical)", body: touchedRels.String(), mandatory: true},
		{heading: "REGIONS", body: regions.String(), mandatory: true},
		{heading: "CONNECTIONS", body: connections.String(), mandatory: true},
		// Optional background, most important first.
		{heading: "RECENT CANONICAL EVENTS", body: recent.String()},
		{heading: "OTHER ENTITIES (canonical)", body: otherEntities.String()},
		{heading: "OTHER RELATIONSHIPS (canonical)", body: otherRels.String()},
	}}
}

// mapKeys lists map keys for the sorting helpers, which make rendered
// prompts stable between runs.
func mapKeys[V any](m map[domain.ID]V) []domain.ID {
	keys := make([]domain.ID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func sortedIDs(keys []domain.ID) []domain.ID {
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func sortedStrings(keys []domain.ID) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = string(k)
	}
	sort.Strings(out)
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
	// DB resolves existence of ids beyond the snapshot's recent-event
	// window (older causes surfacing via late reports). Nil in unit
	// tests, which then fall back to the recent window.
	DB *store.DB
	// AllEvents marks a validator that sees full canonical history
	// (the orchestrator) rather than one region's view.
	AllEvents bool
}

// checkStatusValue rejects set_status values outside the lifecycle
// vocabulary. Enforcing it here (not just at commit) turns a would-be
// dead attempt into a correction the agent can actually fix.
func checkStatusValue(ch domain.Change) error {
	if ch.Op != "set_status" {
		return nil
	}
	v, _ := ch.Patch["status"].(string)
	if domain.ValidStatus(ch.Object, v) {
		return nil
	}
	return fmt.Errorf("%s %s: set_status needs %s; describe nuance in attrs instead",
		ch.Object, ch.ObjectID, strings.Join(domain.StatusVocabulary[ch.Object], " or "))
}

// candidate is the temporary state an event set builds up while it is
// validated in order: objects created (or moved) by earlier changes are
// visible to later ones, exactly as they will be inside the commit
// transaction. The snapshot itself is never mutated.
type candidate struct {
	snap     *store.Snapshot
	entities map[domain.ID]*domain.Entity // created or moved this step
	rels     map[domain.ID]bool
	regions  map[domain.ID]bool
}

func newCandidate(s *store.Snapshot) *candidate {
	return &candidate{snap: s, entities: map[domain.ID]*domain.Entity{},
		rels: map[domain.ID]bool{}, regions: map[domain.ID]bool{}}
}

func (c *candidate) entity(id domain.ID) *domain.Entity {
	if e := c.entities[id]; e != nil {
		return e
	}
	return c.snap.Entities[id]
}

func (c *candidate) region(id domain.ID) bool { return c.regions[id] || c.snap.Regions[id] != nil }

func (c *candidate) rel(id domain.ID) bool { return c.rels[id] || c.snap.Relationships[id] != nil }

var entityKinds = map[string]bool{"person": true, "group": true, "org": true, "settlement": true, "polity": true}

// applyCreation records an upsert in the candidate state, enforcing the
// rules the commit would otherwise only discover inside its
// transaction: complete creations, compatible duplicates, and existing
// endpoints and destinations.
func (c *candidate) applyCreation(ch domain.Change) error {
	if ch.Op != "upsert" {
		return nil
	}
	switch ch.Object {
	case "entity":
		rid := stringPatchValue(ch.Patch, "region_id")
		if rid != "" && !c.region(rid) {
			return fmt.Errorf("entity %q: destination region %q does not exist", ch.ObjectID, rid)
		}
		if e := c.entity(ch.ObjectID); e != nil {
			if k := stringPatchValue(ch.Patch, "kind"); k != "" && k != e.Kind {
				return fmt.Errorf("entity %q already exists as kind %q; an upsert may not change it to %q (choose a new id for a different entity)", ch.ObjectID, e.Kind, k)
			}
			if rid != "" && rid != e.RegionID {
				moved := *e
				moved.RegionID = rid
				c.entities[ch.ObjectID] = &moved
			}
			return nil
		}
		kind, name := stringPatchValue(ch.Patch, "kind"), stringPatchValue(ch.Patch, "name")
		if kind == "" || name == "" {
			return fmt.Errorf("new entity %q needs \"kind\" and \"name\" in its upsert patch", ch.ObjectID)
		}
		if !entityKinds[kind] {
			return fmt.Errorf("new entity %q: kind must be person, group, org, settlement, or polity, got %q", ch.ObjectID, kind)
		}
		c.entities[ch.ObjectID] = &domain.Entity{ID: ch.ObjectID, Kind: kind, Name: name, RegionID: rid, Status: "active"}
	case "relationship":
		if c.rel(ch.ObjectID) {
			return nil
		}
		src, dst := stringPatchValue(ch.Patch, "source"), stringPatchValue(ch.Patch, "target")
		if src == "" || dst == "" || stringPatchValue(ch.Patch, "kind") == "" || src == dst {
			return fmt.Errorf("new relationship %q needs distinct \"source\" and \"target\" entity ids and a \"kind\"", ch.ObjectID)
		}
		for _, id := range []string{src, dst} {
			if c.entity(id) == nil {
				return fmt.Errorf("new relationship %q: entity %q does not exist (create it first, in this or an earlier event)", ch.ObjectID, id)
			}
		}
		c.rels[ch.ObjectID] = true
	case "region":
		if c.region(ch.ObjectID) {
			return nil
		}
		if stringPatchValue(ch.Patch, "name") == "" {
			return fmt.Errorf("new region %q needs a \"name\"", ch.ObjectID)
		}
		c.regions[ch.ObjectID] = true
	}
	return nil
}

func stringPatchValue(patch map[string]any, key string) string {
	v, _ := patch[key].(string)
	return v
}

// CreatedEntities lists entity ids an event set creates, so later
// references in the same reply (belief subjects) can resolve.
func CreatedEntities(s *store.Snapshot, evs []domain.PropEvent) map[domain.ID]bool {
	out := map[domain.ID]bool{}
	for _, ev := range evs {
		for _, ch := range ev.Changes {
			if ch.Object == "entity" && ch.Op == "upsert" && s.Entities[ch.ObjectID] == nil {
				out[ch.ObjectID] = true
			}
		}
	}
	return out
}

// ValidateLogographerProposal checks one event on its own (against the
// snapshot only). Replies are validated as a set with
// ValidateLogographerProposals.
func (pv *ProposalValidator) ValidateLogographerProposal(authorRegion domain.ID, ev *domain.PropEvent) error {
	return pv.validateLogographer(authorRegion, ev, newCandidate(pv.Snap))
}

// ValidateLogographerProposals validates one reply's events in order
// against a temporary state: an entity created by an upsert may be named
// in the same event's affects_entities and changed by later events.
func (pv *ProposalValidator) ValidateLogographerProposals(authorRegion domain.ID, evs []domain.PropEvent) error {
	c := newCandidate(pv.Snap)
	for i := range evs {
		if err := pv.validateLogographer(authorRegion, &evs[i], c); err != nil {
			return fmt.Errorf("event %d (%q): %w", i, evs[i].Title, err)
		}
	}
	return nil
}

// validateLogographer enforces regional authority (FR-021): an agent
// may change only its own region and entities, and may not touch
// directives.
func (pv *ProposalValidator) validateLogographer(authorRegion domain.ID, ev *domain.PropEvent, c *candidate) error {
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
		switch ch.Op {
		case "set_attrs", "upsert", "set_status":
		default:
			return fmt.Errorf("invalid op %q", ch.Op)
		}
		switch ch.Object {
		case "region":
			if ch.ObjectID != authorRegion {
				return fmt.Errorf("region authority: you may not change %q (FR-021)", ch.ObjectID)
			}
		case "entity":
			e := c.entity(ch.ObjectID)
			if e == nil {
				if ch.Op != "upsert" {
					return fmt.Errorf("entity %q does not exist (an upsert creates it; later events may then change it)", ch.ObjectID)
				}
				if rid := stringPatchValue(ch.Patch, "region_id"); rid != "" && rid != authorRegion {
					return fmt.Errorf("entity upsert %q must be located in your region", ch.ObjectID)
				}
			} else if e.RegionID != authorRegion {
				return fmt.Errorf("entity %q is not yours to change", ch.ObjectID)
			} else if rid, _ := ch.Patch["region_id"].(string); rid != "" {
				// Movement: only upsert updates the canonical location.
				if ch.Op == "set_attrs" {
					return fmt.Errorf("entity %q: set_attrs may not set region_id; use upsert to move", ch.ObjectID)
				}
			}
		case "relationship":
			if !c.rel(ch.ObjectID) && ch.Op != "upsert" {
				return fmt.Errorf("relationship %q does not exist", ch.ObjectID)
			}
		case "directive":
			return fmt.Errorf("directives may not be changed by regional agents")
		default:
			return fmt.Errorf("unknown object kind %q", ch.Object)
		}
		if err := checkStatusValue(ch); err != nil {
			return err
		}
		if err := c.applyCreation(ch); err != nil {
			return err
		}
	}
	for _, id := range ev.AffectsEntities {
		if c.entity(id) == nil {
			return fmt.Errorf("unknown entity %q", id)
		}
	}
	for _, id := range ev.Causes {
		if !pv.causeVisibleTo(authorRegion, id) {
			return fmt.Errorf("cause %q is not in your knowledge: cite events your region witnessed or reports delivered to you", id)
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
	if err := pv.checkTravel([]domain.ID{authorRegion}, ev); err != nil {
		return err
	}
	return pv.checkAccess(authorRegion, ev)
}

// ValidateFinalEvent checks one orchestrator event on its own (against
// the snapshot only). The merged history is validated as a set with
// ValidateFinalEvents.
func (pv *ProposalValidator) ValidateFinalEvent(ev *domain.PropEvent) error {
	return pv.validateFinal(ev, newCandidate(pv.Snap))
}

// ValidateFinalEvents validates the merged event set in commit order
// against a temporary state, so creations, later references, moves, and
// duplicate creations are checked before the transaction — where a
// mistake would kill the attempt instead of feeding a correction.
func (pv *ProposalValidator) ValidateFinalEvents(evs []domain.PropEvent) error {
	c := newCandidate(pv.Snap)
	for i := range evs {
		if err := pv.validateFinal(&evs[i], c); err != nil {
			return fmt.Errorf("event %d (%q): %w", i, evs[i].Title, err)
		}
	}
	return nil
}

// validateFinal checks orchestrator output structurally. Cross-region
// changes are allowed for the orchestrator, but every id must resolve
// and deliveries must respect the clock.
func (pv *ProposalValidator) validateFinal(ev *domain.PropEvent, c *candidate) error {
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
		switch ch.Op {
		case "set_attrs", "upsert", "set_status":
		default:
			return fmt.Errorf("invalid op %q", ch.Op)
		}
		switch ch.Object {
		case "region":
			if !c.region(ch.ObjectID) && ch.Op != "upsert" {
				return fmt.Errorf("region %q does not exist", ch.ObjectID)
			}
		case "entity":
			e := c.entity(ch.ObjectID)
			if e == nil && ch.Op != "upsert" {
				return fmt.Errorf("entity %q does not exist (an upsert creates it; later events may then change it)", ch.ObjectID)
			}
			if _, has := ch.Patch["region_id"]; has && e != nil && ch.Op == "set_attrs" {
				return fmt.Errorf("entity %q: set_attrs may not set region_id; use upsert to move", ch.ObjectID)
			}
		case "relationship":
			if !c.rel(ch.ObjectID) && ch.Op != "upsert" {
				return fmt.Errorf("relationship %q does not exist", ch.ObjectID)
			}
		case "directive":
			if pv.Snap.Directives[ch.ObjectID] == nil {
				return fmt.Errorf("directive %q does not exist", ch.ObjectID)
			}
		default:
			return fmt.Errorf("unknown object kind %q", ch.Object)
		}
		if err := checkStatusValue(ch); err != nil {
			return err
		}
		if err := c.applyCreation(ch); err != nil {
			return err
		}
	}
	for _, id := range ev.AffectsEntities {
		if c.entity(id) == nil {
			return fmt.Errorf("unknown entity %q", id)
		}
	}
	for _, id := range ev.Causes {
		if !pv.causeExists(id) {
			return fmt.Errorf("unknown cause %q", id)
		}
	}
	// Causal edges are required when they exist, not as a formality:
	// a development rooted in ongoing conditions or initial states may
	// legitimately carry none. Invented links are rejected by the
	// existence check above.
	for region, step := range ev.Delivery {
		if _, ok := pv.Snap.Regions[region]; !ok {
			return fmt.Errorf("delivery to unknown region %q", region)
		}
		if step <= newStep {
			return fmt.Errorf("delivery step %d must be greater than %d", step, newStep)
		}
	}
	origin := ev.Origin
	if origin == "" || origin == "orchestrator" {
		origin = ""
		if len(ev.Observers) == 0 {
			return fmt.Errorf("event %q has no regional origin, so it needs \"observers\": the regions whose people witness it (an unwitnessed event is known to no one until a report arrives)", ev.Title)
		}
	}
	if err := pv.checkAccess(origin, ev); err != nil {
		return err
	}
	// News travels from whoever witnessed the event.
	witnesses := append([]domain.ID{}, ev.Observers...)
	if origin != "" {
		witnesses = append(witnesses, origin)
	}
	return pv.checkTravel(witnesses, ev)
}

func knownCause(s *store.Snapshot, id domain.ID) bool {
	for _, ev := range s.RecentEvents {
		if ev.ID == id {
			return true
		}
	}
	return false
}

// causeExists checks canonical existence, beyond the snapshot's recent
// window when a DB is available: an event surfacing only through a
// late report is still real history.
func (pv *ProposalValidator) causeExists(id domain.ID) bool {
	if knownCause(pv.Snap, id) {
		return true
	}
	if pv.DB == nil {
		return false
	}
	_, err := pv.DB.GetEvent(context.Background(), id)
	return err == nil
}

// causeVisibleTo separates access from existence: a regional agent may
// cite causes it could actually know — events its region witnessed, or
// events delivered to it by report — however old they are.
func (pv *ProposalValidator) causeVisibleTo(regionID domain.ID, id domain.ID) bool {
	if pv.AllEvents {
		return pv.causeExists(id)
	}
	return pv.CanObserveEvent(regionID, id)
}

// CanObserveEvent reports whether a region could know a committed event
// at the snapshot boundary: an observation record (its region witnessed
// it) or a report already delivered to it. The access record is
// persistent, so an event does not stop being known by aging out of
// the prompt's recent window. Being affected by an event is not enough:
// geography is not access.
func (pv *ProposalValidator) CanObserveEvent(regionID domain.ID, eventID domain.ID) bool {
	if _, ok := pv.Snap.Observed[regionID][eventID]; ok {
		return true
	}
	for _, rep := range pv.Snap.DeliveredReports[regionID] {
		if rep.EventID == eventID {
			return true
		}
	}
	return false
}

// checkTravel enforces baseline news timing: a report cannot arrive
// before the shortest route from any witness allows, measured in whole
// steps of the configured length. Beating the baseline needs an
// explicit "expedited" mechanism, which the orchestrator adjudicates;
// with no known route at all, news travels only by such a mechanism.
func (pv *ProposalValidator) checkTravel(witnesses []domain.ID, ev *domain.PropEvent) error {
	if len(ev.Delivery) == 0 && len(ev.Expedited) == 0 {
		return nil
	}
	for r, why := range ev.Expedited {
		if _, ok := ev.Delivery[r]; !ok {
			return fmt.Errorf("expedited entry for %q has no matching delivery", r)
		}
		if strings.TrimSpace(why) == "" || len(why) > 200 {
			return fmt.Errorf("expedited mechanism for %q must name the means in at most 200 characters", r)
		}
	}
	newStep := pv.CurrentStep + 1
	days := knowledge.TravelDays(pv.Snap, witnesses)
	for _, r := range sortedStrings(mapKeys(ev.Delivery)) {
		step := ev.Delivery[r]
		if contains(witnesses, r) || ev.Expedited[r] != "" {
			continue
		}
		d, ok := days[r]
		if !ok {
			return fmt.Errorf("delivery to %q: no known travel route reaches it from %s; news cannot arrive unless an \"expedited\" mechanism is named", r, strings.Join(witnesses, ", "))
		}
		if min := knowledge.EarliestDeliveryStep(newStep, d, pv.Snap.Meta.StepDays); step < min {
			return fmt.Errorf("delivery to %q at step %d is too early: the route takes %d days and one step is %d days, so the earliest step is %d (or name an \"expedited\" mechanism)", r, step, d, pv.Snap.Meta.StepDays, min)
		}
	}
	return nil
}

func contains(ids []domain.ID, id domain.ID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// maxAccountBytes bounds one limited account.
const maxAccountBytes = 600

// checkAccess validates an event's observers and limited accounts. The
// origin always witnesses its own event in full, so it is neither an
// observer entry nor an account key.
func (pv *ProposalValidator) checkAccess(origin domain.ID, ev *domain.PropEvent) error {
	obs := map[string]bool{}
	for _, r := range ev.Observers {
		if pv.Snap.Regions[r] == nil {
			return fmt.Errorf("observer %q is not a region", r)
		}
		if r == origin {
			return fmt.Errorf("observer %q is the origin, which always witnesses its own event; list only other regions", r)
		}
		obs[r] = true
	}
	for r, text := range ev.Accounts {
		if !obs[r] {
			if _, ok := ev.Delivery[r]; !ok {
				return fmt.Errorf("account for %q: that region is neither an observer nor a delivery target of the event", r)
			}
		}
		if strings.TrimSpace(text) == "" || len(text) > maxAccountBytes {
			return fmt.Errorf("account for %q must be present and at most %d bytes", r, maxAccountBytes)
		}
	}
	return nil
}

// ValidateReaction checks one reaction request: the region must exist
// and every premise event must be observable by that region now.
func (pv *ProposalValidator) ValidateReaction(regionID domain.ID, rx ReactionRequest) error {
	if pv.Snap.Regions[regionID] == nil {
		return fmt.Errorf("reaction target %q is not a region", regionID)
	}
	if len(rx.Premise) == 0 {
		return fmt.Errorf("reaction for %q needs a premise: name at least one canonical event that region witnessed or received by report", regionID)
	}
	for _, id := range rx.Premise {
		if !pv.CanObserveEvent(regionID, id) {
			return fmt.Errorf("reaction for %q: premise event %q is not knowable by that region yet (events it witnessed or reports delivered to it only)", regionID, id)
		}
	}
	return nil
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
