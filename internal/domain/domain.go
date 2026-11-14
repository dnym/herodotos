// Package domain defines the canonical entities of a simulated world:
// regions, entities, events, proposals, and the stable identifiers that
// tie them together across time.
package domain

import (
	"fmt"
	"regexp"
	"time"
)

// ID is a stable identifier for a world object. Identities persist across
// location, affiliation, and status changes.
type ID = string

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidID reports whether s is a well-formed identifier.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Meta holds world-level metadata and the simulated clock. Simulated time
// never derives from the host clock.
type Meta struct {
	WorldID   string `json:"world_id"`
	WorldName string `json:"world_name"`
	// StartDate is the day the simulation clock begins (YYYY-MM-DD).
	StartDate string `json:"start_date"`
	// StepDays is the fixed duration of one time step in days.
	StepDays int `json:"step_days"`
	// ClockStep is the number of committed steps; 0 means the start state.
	ClockStep int `json:"clock_step"`
	// Revision increments once per committed step and guards concurrent
	// writers during commitment.
	Revision int64 `json:"revision"`
}

// StepDate returns the display date at the given step boundary.
func (m Meta) StepDate(step int) (time.Time, error) {
	t, err := time.Parse("2006-01-02", m.StartDate)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid start date %q: %w", m.StartDate, err)
	}
	return t.AddDate(0, 0, step*m.StepDays), nil
}

// Region is a geographical area with a stable identity independent of the
// agent currently managing it.
type Region struct {
	ID     ID             `json:"id"`
	Name   string         `json:"name"`
	Status string         `json:"status"` // active, dormant
	Attrs  map[string]any `json:"attrs"`
}

// Connection is a travel link between two regions. TravelDays is the
// baseline delay for information to cross it.
type Connection struct {
	A         ID  `json:"a"`
	B         ID  `json:"b"`
	TravelDay int `json:"travel_days"`
}

// Entity is a historically relevant person, group, organization,
// settlement, or polity.
type Entity struct {
	ID       ID             `json:"id"`
	Kind     string         `json:"kind"` // person, group, org, settlement, polity
	Name     string         `json:"name"`
	RegionID ID             `json:"region_id"` // empty when no fixed location
	Status   string         `json:"status"`
	Attrs    map[string]any `json:"attrs"`
}

// Relationship links two entities (allegiance, family, hostility, trade...).
type Relationship struct {
	ID       ID             `json:"id"`
	SourceID ID             `json:"source_id"`
	TargetID ID             `json:"target_id"`
	Kind     string         `json:"kind"`
	Status   string         `json:"status"` // active, ended
	Attrs    map[string]any `json:"attrs"`
}

// Directive is a human creative instruction. Free-text directives default
// to soft; hard directives are binding unless impossible.
type Directive struct {
	ID     ID     `json:"id"`
	Text   string `json:"text"`
	Hard   bool   `json:"hard"`
	Status string `json:"status"` // pending, active, fulfilled, conflicting
	Notes  string `json:"notes"`
}

// Change describes a proposed or committed modification of one world
// object. Patch keys are merged into the object's attributes; a null value
// removes the key.
type Change struct {
	Object   string         `json:"object"` // region, entity, relationship, directive
	ObjectID ID             `json:"object_id"`
	Op       string         `json:"op"` // set_attrs, upsert, set_status
	Patch    map[string]any `json:"patch"`
}

// PropEvent is one event inside an agent proposal: an intention, not a
// committed outcome.
type PropEvent struct {
	Type            string         `json:"type"`
	Title           string         `json:"title"`
	Prose           string         `json:"prose"`
	Origin          ID             `json:"origin,omitempty"` // authoring region, set by the engine
	Causes          []ID           `json:"causes,omitempty"`
	Changes         []Change       `json:"changes,omitempty"`
	AffectsEntities []ID           `json:"affects_entities,omitempty"`
	// Observers are the regions (besides the origin) whose people
	// witness the event directly as it happens. Being affected is not
	// observing: the target of a covert act is no observer.
	Observers []ID `json:"observers,omitempty"`
	// Accounts gives, per observer or report recipient, the limited
	// version that region perceives or is told, instead of the full
	// canonical prose.
	Accounts map[string]string `json:"accounts,omitempty"`
	Delivery map[string]int    `json:"delivery,omitempty"` // region -> earliest step
}

// Proposal is an agent's uncommitted output for one step.
type Proposal struct {
	ID     ID
	Author string // logographer:<region-id> or orchestrator
	Kind   string // development, reaction, none
	Prose  string
	Events []PropEvent
}

// Event is a committed canonical happening.
type Event struct {
	ID        ID     `json:"id"`
	Step      int    `json:"step"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Prose     string `json:"prose"`
	RegionIDs []ID   `json:"region_ids"`
	EntityIDs []ID   `json:"entity_ids"`
	CauseIDs  []ID   `json:"cause_ids"`
}

// Report carries an event's news to a region at a plausible earliest step.
type Report struct {
	ID             ID  `json:"id"`
	EventID        ID  `json:"event_id"`
	TargetRegionID ID  `json:"target_region_id"`
	EarliestStep   int `json:"earliest_step"`
	DeliveredStep  int `json:"delivered_step"` // 0 when not delivered yet
	// Account is the limited version the report carries; empty means
	// the full canonical prose.
	Account string `json:"account,omitempty"`
}

// Belief is an in-world belief held by a region's relevant inhabitants or
// a specific subject; it is distinct from canonical truth and from the
// information given to an agent.
type Belief struct {
	ID        ID     `json:"id"`
	RegionID  ID     `json:"region_id"`
	SubjectID ID     `json:"subject_id"` // empty for the region's general belief
	Modality  string `json:"modality"`
	Statement string `json:"statement"`
	AsOfStep  int    `json:"as_of_step"`
}

// Memory is a compact persistent summary for one region's agent.
type Memory struct {
	RegionID    ID       `json:"region_id"`
	Summary     string   `json:"summary"`
	OpenThreads []string `json:"open_threads"`
	UpdatedStep int      `json:"updated_step"`
}

// StatusVocabulary lists the allowed set_status values per object
// kind. Status is a lifecycle flag, not a description: narrative
// color ("under negotiation", "tense") belongs in attrs. The commit
// layer enforces exactly this vocabulary, so validators can reject
// bad values while the agent can still correct them.
var StatusVocabulary = map[string][]string{
	"region":       {"active", "dormant"},
	"entity":       {"active", "inactive", "destroyed"},
	"relationship": {"active", "ended"},
}

// ValidStatus reports whether status is allowed for the object kind.
func ValidStatus(object, status string) bool {
	for _, s := range StatusVocabulary[object] {
		if s == status {
			return true
		}
	}
	return false
}

// ReferenceDoc is human-authored reference material scoped to the world
// or one region: binding rules or advisory knowledge (a skill-style doc
// whose description is always shown and whose body is loaded on match
// or on request).
type ReferenceDoc struct {
	ID          ID     `json:"id"`
	Scope       string `json:"scope"` // world, region
	RegionID    ID     `json:"region_id"`
	Binding     bool   `json:"binding"`
	Enabled     bool   `json:"enabled"`
	Title       string `json:"title"`
	Description string `json:"description"` // one line; always visible to scoped agents
	Body        string `json:"body"`
	Keywords    string `json:"keywords"`
}

// Attempt groups all diagnostics of one step execution.
type Attempt struct {
	ID          ID     `json:"id"`
	Step        int    `json:"step"` // the step boundary this attempt advances to
	Status      string `json:"status"`
	Error       string `json:"error"`
	PauseReason string `json:"pause_reason"` // set when this attempt halted the world
}
