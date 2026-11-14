package store

import (
	"context"
	"sort"

	"github.com/dnym/herodotos/internal/domain"
)

// ExportBundle is the complete canonical world as one JSON document:
// current state plus the full event history. It deliberately excludes
// attempt diagnostics (engineering traces, not world content), the
// per-field state-change audit log (derivable by replaying events in
// order), and disabled reference documents (kept in the database but
// intentionally out of every agent view). The durable pause flag is
// included: it is part of the world's current condition.
type ExportBundle struct {
	GenTool       string                `json:"gen_tool"`
	Regions       []domain.Region       `json:"regions"`
	Connections   []domain.Connection   `json:"connections"`
	Entities      []domain.Entity       `json:"entities"`
	Relationships []domain.Relationship `json:"relationships"`
	Directives    []domain.Directive    `json:"directives"`
	DirectiveLog  []DirectiveChange     `json:"directive_log"`
	References    []domain.ReferenceDoc `json:"references"`
	Events        []domain.Event        `json:"events"`
	Reports       []domain.Report       `json:"reports"`
	Observations  []ExportObservation   `json:"observations"`
	Beliefs       []domain.Belief       `json:"beliefs"`
	Memories      []domain.Memory       `json:"memories"`
	Meta          domain.Meta           `json:"meta"`
	Pause         *ExportPause          `json:"pause,omitempty"`
}

// ExportPause records a durable world pause, if one is in effect.
type ExportPause struct {
	Reason string `json:"reason"`
}

// ExportObservation records that a region witnessed an event directly,
// with the limited account it received (empty: the full prose).
type ExportObservation struct {
	EventID  domain.ID `json:"event_id"`
	RegionID domain.ID `json:"region_id"`
	Account  string    `json:"account,omitempty"`
}

// ExportBundle reads the entire committed world in chronological order.
// Every read runs inside one transaction, so the bundle is a
// consistent snapshot: it is entirely before or entirely after any
// concurrent commit, never a mixture.
func (d *DB) ExportBundle(ctx context.Context) (*ExportBundle, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	snap, err := d.snapshotTx(ctx, tx, 0)
	if err != nil {
		return nil, err
	}
	b := &ExportBundle{GenTool: "herodotos", Meta: snap.Meta}

	for _, id := range sortedDomainKeys(snap.Regions) {
		b.Regions = append(b.Regions, *snap.Regions[id])
	}
	b.Connections = snap.Connections
	for _, id := range sortedDomainKeys(snap.Entities) {
		b.Entities = append(b.Entities, *snap.Entities[id])
	}
	for _, id := range sortedDomainKeys(snap.Relationships) {
		b.Relationships = append(b.Relationships, *snap.Relationships[id])
	}
	for _, id := range sortedDomainKeys(snap.Directives) {
		b.Directives = append(b.Directives, *snap.Directives[id])
	}
	if b.DirectiveLog, err = listDirectiveLog(ctx, tx); err != nil {
		return nil, err
	}
	b.References = append(b.References, snap.WorldRefs...)
	for _, id := range sortedDomainKeys(snap.RegionRefs) {
		b.References = append(b.References, snap.RegionRefs[id]...)
	}

	events, err := listEvents(ctx, tx, EventQuery{Limit: 1 << 30})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Step != events[j].Step {
			return events[i].Step < events[j].Step
		}
		return events[i].ID < events[j].ID
	})
	b.Events = events

	rows, err := tx.QueryContext(ctx,
		`SELECT id, event_id, target_region_id, earliest_step, COALESCE(delivered_step, 0), COALESCE(account, ''), COALESCE(mechanism, '')
		 FROM reports ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rp domain.Report
		if err := rows.Scan(&rp.ID, &rp.EventID, &rp.TargetRegionID, &rp.EarliestStep, &rp.DeliveredStep, &rp.Account, &rp.Mechanism); err != nil {
			return nil, err
		}
		b.Reports = append(b.Reports, rp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	orows, err := tx.QueryContext(ctx,
		`SELECT event_id, region_id, COALESCE(account, '') FROM observations ORDER BY event_id, region_id`)
	if err != nil {
		return nil, err
	}
	defer orows.Close()
	for orows.Next() {
		var ob ExportObservation
		if err := orows.Scan(&ob.EventID, &ob.RegionID, &ob.Account); err != nil {
			return nil, err
		}
		b.Observations = append(b.Observations, ob)
	}
	if err := orows.Err(); err != nil {
		return nil, err
	}

	for _, regionID := range sortedDomainKeys(snap.Beliefs) {
		b.Beliefs = append(b.Beliefs, snap.Beliefs[regionID]...)
	}
	for _, id := range sortedDomainKeys(snap.Memories) {
		b.Memories = append(b.Memories, *snap.Memories[id])
	}
	reason, paused, err := worldPauseTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if paused {
		b.Pause = &ExportPause{Reason: reason}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return b, nil
}

func sortedDomainKeys[V any](m map[domain.ID]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
