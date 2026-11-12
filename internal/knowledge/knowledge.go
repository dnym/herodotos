// Package knowledge enforces the separation of canonical truth,
// agent-accessible information, and in-world knowledge. Context views are
// built under a visibility policy: a regional historian sees its own
// region's canonical conditions, delivered reports, recorded beliefs,
// memories, and scoped references — never another region's private
// canonical state.
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
	Reports       []ReportNote // news from other regions, already delivered
	Entities      []*domain.Entity
	Relationships []*domain.Relationship // among visible entities
	Directives    []domain.Directive
	Bindings      []domain.ReferenceDoc // always shown in full: world rules
	Retrieved     []domain.ReferenceDoc // bodies auto-loaded by keyword match
	Catalog       []domain.ReferenceDoc // id/title/description only: ask to consult
	RecentEvents  []domain.Event        // canonical events involving this region
	AdjacentIDs   []domain.ID           // neighbor identity only, no conditions
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
		v.Directives = append(v.Directives, *d)
	}
	for _, rep := range s.DeliveredReports[regionID] {
		note := ReportNote{EventID: rep.EventID, DeliveredStep: rep.DeliveredStep}
		if ev, err := lookup.GetEvent(ctx, rep.EventID); err == nil {
			note.Title, note.Prose, note.EventStep = ev.Title, ev.Prose, ev.Step
		} else {
			note.Title = rep.EventID
		}
		v.Reports = append(v.Reports, note)
	}
	for _, ev := range s.RecentEvents {
		for _, rid := range ev.RegionIDs {
			if rid == regionID {
				v.RecentEvents = append(v.RecentEvents, ev)
				break
			}
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
	return v, nil
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
