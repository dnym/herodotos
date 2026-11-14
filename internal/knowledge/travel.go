package knowledge

import (
	"sort"

	"github.com/dnym/herodotos/internal/domain"
	"github.com/dnym/herodotos/internal/store"
)

// TravelDays returns the shortest known travel time in days from any of
// the given regions to every reachable region, over the connection
// graph. Unreachable regions are absent: news has no known route there.
// This is a baseline timing rule, not a transport simulation.
func TravelDays(s *store.Snapshot, from []domain.ID) map[domain.ID]int {
	adj := map[domain.ID][]domain.Connection{}
	for _, c := range s.Connections {
		adj[c.A] = append(adj[c.A], c)
		adj[c.B] = append(adj[c.B], c)
	}
	dist := map[domain.ID]int{}
	done := map[domain.ID]bool{}
	for _, r := range from {
		dist[r] = 0
	}
	// The graphs are tiny; a quadratic Dijkstra keeps this obvious.
	for {
		cur, best := domain.ID(""), -1
		for id, d := range dist {
			if !done[id] && (best < 0 || d < best || (d == best && id < cur)) {
				cur, best = id, d
			}
		}
		if best < 0 {
			return dist
		}
		done[cur] = true
		for _, c := range adj[cur] {
			next := c.B
			if next == cur {
				next = c.A
			}
			if d, ok := dist[next]; !ok || best+c.TravelDay < d {
				dist[next] = best + c.TravelDay
			}
		}
	}
}

// EarliestDeliveryStep is the first step at which news of an event
// happening during eventStep can arrive after travelling days: at least
// one step later, and one more step for every started step length of
// travel.
func EarliestDeliveryStep(eventStep, days, stepDays int) int {
	if stepDays <= 0 {
		stepDays = 1
	}
	steps := (days + stepDays - 1) / stepDays
	if steps < 1 {
		steps = 1
	}
	return eventStep + steps
}

// TravelNote is one destination's baseline news timing from a region.
type TravelNote struct {
	RegionID     domain.ID
	Days         int
	Neighbour    bool
	EarliestStep int // for news of an event in the step about to begin
}

// travelNotes lists every other reachable region with its baseline
// timing, nearest first.
func travelNotes(s *store.Snapshot, regionID domain.ID) []TravelNote {
	newStep := s.Meta.ClockStep + 1
	neighbour := map[domain.ID]bool{}
	for _, c := range s.Connections {
		if c.A == regionID {
			neighbour[c.B] = true
		} else if c.B == regionID {
			neighbour[c.A] = true
		}
	}
	var out []TravelNote
	for id, d := range TravelDays(s, []domain.ID{regionID}) {
		if id == regionID {
			continue
		}
		out = append(out, TravelNote{RegionID: id, Days: d, Neighbour: neighbour[id],
			EarliestStep: EarliestDeliveryStep(newStep, d, s.Meta.StepDays)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Days != out[j].Days {
			return out[i].Days < out[j].Days
		}
		return out[i].RegionID < out[j].RegionID
	})
	return out
}
