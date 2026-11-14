// Package knowledge enforces the separation of canonical truth,
// agent-accessible information, and in-world knowledge. Context views are
// built under a visibility policy: a regional historian sees its own
// region's canonical conditions, the events its region witnessed
// (observations), delivered reports, recorded beliefs, memories, and
// scoped references — never another region's private canonical state.
// Geography is not access: an event that affects a region's entities
// is known there only through an observation or a delivered report, and
// then only in the account that observation or report carries.
package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/store"
)

// ReportNote is a delivered report joined with its event text.
type ReportNote struct {
	EventID       string
	Title         string
	Prose         string
	EventStep     int
	DeliveredStep int
}

// LogographerView is everything one regional agent may know at this
// boundary.
type LogographerView struct {
	Region        *domain.Region
	Memory        *domain.Memory
	Beliefs       []domain.Belief
	Reports       []ReportNote // newest first, capped (see maxDeliveredReports)
	ReportsHidden int          // older delivered reports left out of the prompt
	Entities      []*domain.Entity
	Relationships []*domain.Relationship // among visible entities
	Directives    []domain.Directive
	Bindings      []domain.ReferenceDoc // always shown in full: world rules
	Retrieved     []domain.ReferenceDoc // bodies auto-loaded by keyword match
	Catalog       []domain.ReferenceDoc // id/title/description only: ask to consult
	RecentEvents  []domain.Event        // recent events this region witnessed, as it knows them
	AdjacentIDs   []domain.ID           // neighbor identity only, no conditions
	Travel        []TravelNote          // baseline news timing to reachable regions
	StepDays      int
}

// EventLookup resolves an event id; *store.DB.GetEvent satisfies it.
type EventLookup interface {
	GetEvent(ctx context.Context, id domain.ID) (domain.Event, error)
}

// BuildLogographerView assembles a visibility-scoped view from a snapshot.
func BuildLogographerView(ctx context.Context, s *store.Snapshot, regionID domain.ID, lookup EventLookup) (*LogographerView, error) {
	v := &LogographerView{Region: s.Regions[regionID]}
	if v.Region == nil {
		return nil, &UnknownRegionError{ID: regionID}
	}
	v.Memory = s.Memories[regionID]
	v.Beliefs = s.Beliefs[regionID]

	visible := map[domain.ID]bool{}
	for _, e := range s.Entities {
		if e.RegionID == regionID {
			v.Entities = append(v.Entities, e)
			visible[e.ID] = true
		}
	}
	for _, r := range s.Relationships {
		if visible[r.SourceID] || visible[r.TargetID] {
			v.Relationships = append(v.Relationships, r)
		}
	}
	for _, d := range s.Directives {
		if d.Status == "withdrawn" {
			continue // on record for humans, retired for agents
		}
		v.Directives = append(v.Directives, *d)
	}
	sort.Slice(v.Directives, func(i, j int) bool { return v.Directives[i].ID < v.Directives[j].ID })

	// Delivered reports arrive oldest-first from the snapshot. The
	// prompt shows the freshest news first and bounds the set: old
	// news is already woven into memory and recent events, and
	// unbounded accumulation would crowd out current decisions.
	// Hidden reports remain citable: their events are canonical and
	// stay valid causes.
	notes := make([]ReportNote, 0, len(s.DeliveredReports[regionID]))
	for _, rep := range s.DeliveredReports[regionID] {
		note := ReportNote{EventID: rep.EventID, DeliveredStep: rep.DeliveredStep}
		if ev, err := lookup.GetEvent(ctx, rep.EventID); err == nil {
			ev = AsKnown(ev, rep.Account)
			note.Title, note.Prose, note.EventStep = ev.Title, ev.Prose, ev.Step
		} else {
			note.Title = rep.EventID
		}
		notes = append(notes, note)
	}
	sort.SliceStable(notes, func(i, j int) bool {
		if notes[i].DeliveredStep != notes[j].DeliveredStep {
			return notes[i].DeliveredStep > notes[j].DeliveredStep
		}
		return notes[i].EventID > notes[j].EventID
	})
	if len(notes) > maxDeliveredReports {
		v.ReportsHidden = len(notes) - maxDeliveredReports
		notes = notes[:maxDeliveredReports]
	}
	v.Reports = notes

	for _, ev := range s.RecentEvents {
		if account, ok := s.Observed[regionID][ev.ID]; ok {
			v.RecentEvents = append(v.RecentEvents, AsKnown(ev, account))
		}
	}
	classifyReferences(v, s, regionID)
	for _, c := range s.Connections {
		if c.A == regionID {
			v.AdjacentIDs = append(v.AdjacentIDs, c.B)
		} else if c.B == regionID {
			v.AdjacentIDs = append(v.AdjacentIDs, c.A)
		}
	}
	v.Travel = travelNotes(s, regionID)
	v.StepDays = s.Meta.StepDays
	sort.Slice(v.Entities, func(i, j int) bool { return v.Entities[i].ID < v.Entities[j].ID })
	sort.Slice(v.Relationships, func(i, j int) bool { return v.Relationships[i].ID < v.Relationships[j].ID })
	sort.Slice(v.Beliefs, func(i, j int) bool { return v.Beliefs[i].ID < v.Beliefs[j].ID })
	sort.Slice(v.RecentEvents, func(i, j int) bool {
		if v.RecentEvents[i].Step != v.RecentEvents[j].Step {
			return v.RecentEvents[i].Step > v.RecentEvents[j].Step
		}
		return v.RecentEvents[i].ID > v.RecentEvents[j].ID
	})
	return v, nil
}

// AsKnown returns the event as a region knows it. With a limited
// account, the canonical title, type, and prose are replaced: a title
// like "Plot against the queen" would otherwise disclose exactly what
// the account withholds. Ids stay, so the event remains citable.
func AsKnown(ev domain.Event, account string) domain.Event {
	if account == "" {
		return ev
	}
	ev.Type = "account"
	ev.Title = "What your region learned"
	ev.Prose = account
	return ev
}

// AccountFor returns the account through which a region knows an event:
// its observation if it witnessed it, else a delivered report. ok is
// false when the region does not know the event at all.
func AccountFor(s *store.Snapshot, regionID, eventID domain.ID) (account string, ok bool) {
	if a, seen := s.Observed[regionID][eventID]; seen {
		return a, true
	}
	for _, rep := range s.DeliveredReports[regionID] {
		if rep.EventID == eventID {
			return rep.Account, true
		}
	}
	return "", false
}

// maxRecallQueries and maxRecallResults bound one history lookup.
const (
	maxRecallQueries = 3
	maxRecallResults = 8
)

// Recall answers a region's history lookup from its own access record:
// each query is an event id (exact) or keywords matched against the
// version of the event the region knows — never against canonical
// prose it was not told. Newest matches first, bounded.
func Recall(history []store.HistoryNote, queries []string) []domain.Event {
	if len(queries) > maxRecallQueries {
		queries = queries[:maxRecallQueries]
	}
	var out []domain.Event
	seen := map[domain.ID]bool{}
	for i := len(history) - 1; i >= 0 && len(out) < maxRecallResults; i-- {
		known := AsKnown(history[i].Event, history[i].Account)
		hay := strings.ToLower(known.Title + " " + known.Prose)
		for _, q := range queries {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			match := q == string(known.ID)
			if !match {
				words := refKeywords(q)
				match = len(words) > 0
				for _, w := range words {
					// Crude plural folding: "riots" finds "rioting".
					if len(w) > 3 {
						w = strings.TrimSuffix(w, "s")
					}
					if !strings.Contains(hay, w) {
						match = false
						break
					}
				}
			}
			if match && !seen[known.ID] {
				seen[known.ID] = true
				out = append(out, known)
				break
			}
		}
	}
	return out
}

// UnknownRegionError names a missing region.
type UnknownRegionError struct{ ID domain.ID }

// VisibleReferences lists every reference document the region may see,
// which is exactly the set a reply may consult.
func VisibleReferences(v *LogographerView) []domain.ReferenceDoc {
	var out []domain.ReferenceDoc
	out = append(out, v.Bindings...)
	out = append(out, v.Retrieved...)
	out = append(out, v.Catalog...)
	return out
}

// classifyReferences splits the region-visible references into the
// three prompt tiers of the skill paradigm: binding documents always
// appear in full; advisory documents whose keywords match what the
// region is currently dealing with have their bodies auto-loaded (at
// most maxRetrieved, best match first); everything else appears in the
// catalog as id/title/description only, available on request. The
// scoring is deterministic keyword matching — no inference, no
// randomness.
func classifyReferences(v *LogographerView, s *store.Snapshot, regionID domain.ID) {
	var visible []domain.ReferenceDoc
	visible = append(visible, s.WorldRefs...)
	visible = append(visible, s.RegionRefs[regionID]...)
	if len(visible) == 0 {
		return
	}

	hay := strings.ToLower(strings.Join(regionContextText(v), " "))
	type scored struct {
		ref   domain.ReferenceDoc
		score int
	}
	var matches []scored
	for _, ref := range visible {
		if ref.Binding {
			v.Bindings = append(v.Bindings, ref)
			continue
		}
		score := 0
		for _, kw := range refKeywords(ref.Keywords) {
			if strings.Contains(hay, kw) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, scored{ref, score})
		} else {
			v.Catalog = append(v.Catalog, ref)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].ref.ID < matches[j].ref.ID
	})
	for i, m := range matches {
		if i < maxRetrieved {
			v.Retrieved = append(v.Retrieved, m.ref)
		} else {
			v.Catalog = append(v.Catalog, m.ref)
		}
	}
}

// maxRetrieved bounds how many advisory bodies one prompt may carry.
const maxRetrieved = 3

// maxDeliveredReports bounds how many delivered reports one prompt
// expands. Reports keep their canonical event ids, so omitted older
// ones remain known history the agent can still cite.
const maxDeliveredReports = 12

func regionContextText(v *LogographerView) []string {
	texts := []string{string(v.Region.ID), v.Region.Name}
	for _, a := range v.Region.Attrs {
		texts = append(texts, fmt.Sprint(a))
	}
	for _, e := range v.Entities {
		texts = append(texts, e.Name)
	}
	if v.Memory != nil {
		texts = append(texts, v.Memory.Summary)
		texts = append(texts, v.Memory.OpenThreads...)
	}
	for _, ev := range v.RecentEvents {
		texts = append(texts, ev.Title, ev.Prose)
	}
	return texts
}

// refKeywords splits a reference's keyword string into match tokens:
// lowercase alphanumerics of at least three characters. Two-word
// phrases do not match; each word does, on its own.
func refKeywords(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len(tok) >= 3 {
			out = append(out, tok)
		}
	}
	return out
}

func (e *UnknownRegionError) Error() string { return "unknown region " + e.ID }
