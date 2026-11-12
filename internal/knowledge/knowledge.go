// Package knowledge enforces the separation of canonical truth,
// agent-accessible information, and in-world knowledge. Context views are
// built under a visibility policy: a regional historian sees its own
// region's canonical conditions, delivered reports, recorded beliefs,
// memories, and scoped references — never another region's private
// canonical state.
package knowledge

import (
	"context"

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
	Refs          []domain.ReferenceDoc // world-scoped + region-scoped
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
	v.Refs = append(v.Refs, s.WorldRefs...)
	v.Refs = append(v.Refs, s.RegionRefs[regionID]...)

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

func (e *UnknownRegionError) Error() string { return "unknown region " + e.ID }
