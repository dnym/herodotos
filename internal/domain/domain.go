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
	WorldID   string
	WorldName string
	// StartDate is the day the simulation clock begins (YYYY-MM-DD).
	StartDate string
	// StepDays is the fixed duration of one time step in days.
	StepDays int
	// ClockStep is the number of committed steps; 0 means the start state.
	ClockStep int
	// Revision increments once per committed step and guards concurrent
	// writers during commitment.
	Revision int64
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
	ID     ID
	Name   string
	Status string // active, dormant
	Attrs  map[string]any
}

// Connection is a travel link between two regions. TravelDays is the
// baseline delay for information to cross it.
type Connection struct {
	A         ID
	B         ID
	TravelDay int
}

// Entity is a historically relevant person, group, organization,
// settlement, or polity.
type Entity struct {
	ID       ID
	Kind     string // person, group, org, settlement, polity
	Name     string
	RegionID ID // empty when no fixed location
	Status   string
	Attrs    map[string]any
}

// Relationship links two entities (allegiance, family, hostility, trade...).
type Relationship struct {
	ID       ID
	SourceID ID
	TargetID ID
	Kind     string
	Status   string // active, ended
	Attrs    map[string]any
}

// Directive is a human creative instruction. Free-text directives default
// to soft; hard directives are binding unless impossible.
type Directive struct {
	ID     ID
	Text   string
	Hard   bool
	Status string // pending, active, fulfilled, conflicting
	Notes  string
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
	Causes          []ID           `json:"causes,omitempty"`
	Changes         []Change       `json:"changes,omitempty"`
	AffectsEntities []ID           `json:"affects_entities,omitempty"`
	Delivery        map[string]int `json:"delivery,omitempty"` // region -> earliest step
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
	ID        ID
	Step      int
	Type      string
	Title     string
	Prose     string
	RegionIDs []ID
	EntityIDs []ID
	CauseIDs  []ID
}

// Report carries an event's news to a region at a plausible earliest step.
type Report struct {
	ID             ID
	EventID        ID
	TargetRegionID ID
	EarliestStep   int
	DeliveredStep  int // 0 when not delivered yet
}

// Belief is an in-world belief held by a region's relevant inhabitants or
// a specific subject; it is distinct from canonical truth and from the
// information given to an agent.
type Belief struct {
	ID         ID
	RegionID   ID
	SubjectID  ID // empty for the region's general belief
	Modality   string
	Statement  string
	AsOfStep   int
}

// Memory is a compact persistent summary for one region's agent.
type Memory struct {
	RegionID    ID
	Summary     string
	OpenThreads []string
	UpdatedStep int
}

// ReferenceDoc is a scoped reference document; binding documents state
// world rules, others are descriptive background.
type ReferenceDoc struct {
	ID       ID
	Scope    string // world, region
	RegionID ID
	Binding  bool
	Title    string
	Body     string
	Keywords string
}

// Attempt groups all diagnostics of one step execution.
type Attempt struct {
	ID     ID
	Step   int // the step boundary this attempt advances to
	Status string
	Error  string
}
