package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dnym/herodotos/internal/domain"
)

// CommittedEvent is an outcome the simulation engine validated and the
// orchestrator accepted. The engine assigns its canonical ID.
type CommittedEvent struct {
	Type      string
	Title     string
	Prose     string
	RegionIDs []domain.ID
	EntityIDs []domain.ID
	CauseIDs  []domain.ID
	Changes   []domain.Change
}

// ReportPlan schedules knowledge of a committed event (by index in
// CommitPlan.Events) to reach a region no earlier than EarliestStep.
type ReportPlan struct {
	EventIndex   int
	TargetRegion domain.ID
	EarliestStep int
}

// CommitPlan is one candidate step. Everything in it commits atomically
// or nothing does.
type CommitPlan struct {
	AttemptID    string
	BaseRevision int64
	Summary      string
	Events       []CommittedEvent
	Reports      []ReportPlan
	Memories     []domain.Memory
	Beliefs      []domain.Belief // RegionID, Modality, Statement, SubjectID; the store assigns id and as_of_step
}

// maxBeliefsPerRegion keeps one region's recorded beliefs bounded: beliefs
// inform prompts, so recency matters more than accumulation.
const maxBeliefsPerRegion = 12

// CommitStep commits one step against the given base revision. If the
// world moved since the snapshot, nothing is written and
// ErrStaleRevision is returned.
func (d *DB) CommitStep(ctx context.Context, plan CommitPlan) ([]string, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	meta, err := getMeta(ctx, tx)
	if err != nil {
		return nil, err
	}
	if meta.Revision != plan.BaseRevision {
		return nil, fmt.Errorf("%w: base %d, current %d", ErrStaleRevision, plan.BaseRevision, meta.Revision)
	}
	step := meta.ClockStep + 1

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO steps(step, committed_utc, summary) VALUES (?, ?, ?)`,
		step, utcNow(), plan.Summary); err != nil {
		return nil, fmt.Errorf("steps: %w", err)
	}

	ids := make([]string, 0, len(plan.Events))
	for i, ev := range plan.Events {
		eid, err := nextIDTx(ctx, tx, "evt")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events(id, step, type, title, prose, region_ids, entity_ids, cause_ids)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			eid, step, ev.Type, ev.Title, ev.Prose,
			jsonIDList(ev.RegionIDs), jsonIDList(ev.EntityIDs), jsonIDList(ev.CauseIDs)); err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
		ids = append(ids, eid)
		for _, ch := range ev.Changes {
			if err := applyChangeTx(ctx, tx, eid, ch); err != nil {
				return nil, fmt.Errorf("event %s %q: %w", eid, ev.Title, err)
			}
		}
	}

	for _, rp := range plan.Reports {
		if rp.EventIndex < 0 || rp.EventIndex >= len(ids) {
			return nil, fmt.Errorf("report plan references event index %d", rp.EventIndex)
		}
		if rp.EarliestStep <= step {
			return nil, fmt.Errorf("report for %s must deliver after the current step %d, got %d",
				ids[rp.EventIndex], step, rp.EarliestStep)
		}
		rid, err := nextIDTx(ctx, tx, "rep")
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reports(id, event_id, target_region_id, earliest_step) VALUES (?, ?, ?, ?)`,
			rid, ids[rp.EventIndex], rp.TargetRegion, rp.EarliestStep); err != nil {
			return nil, fmt.Errorf("report: %w", err)
		}
	}

	for _, mem := range plan.Memories {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memories(region_id, summary, open_threads, updated_step) VALUES (?, ?, ?, ?)
			 ON CONFLICT(region_id) DO UPDATE SET summary=excluded.summary, open_threads=excluded.open_threads, updated_step=excluded.updated_step`,
			mem.RegionID, mem.Summary, jsonStrings(mem.OpenThreads), mem.UpdatedStep); err != nil {
			return nil, fmt.Errorf("memory %s: %w", mem.RegionID, err)
		}
	}

	if len(plan.Beliefs) > 0 {
		regions := map[domain.ID]bool{}
		for _, bl := range plan.Beliefs {
			bid, err := nextIDTx(ctx, tx, "bel")
			if err != nil {
				return nil, err
			}
			var subject any
			if bl.SubjectID != "" {
				subject = bl.SubjectID
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO beliefs(id, region_id, subject_id, modality, statement, as_of_step)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				bid, bl.RegionID, subject, bl.Modality, bl.Statement, step); err != nil {
				return nil, fmt.Errorf("belief: %w", err)
			}
			regions[bl.RegionID] = true
		}
		for region := range regions {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM beliefs WHERE region_id = ? AND id NOT IN
				 (SELECT id FROM beliefs WHERE region_id = ? ORDER BY id DESC LIMIT ?)`,
				region, region, maxBeliefsPerRegion); err != nil {
				return nil, fmt.Errorf("prune beliefs %s: %w", region, err)
			}
		}
	}

	if plan.AttemptID != "" {
		res, err := tx.ExecContext(ctx,
			`UPDATE attempts SET status='committed', ended_utc=? WHERE id=? AND status='running'`,
			utcNow(), plan.AttemptID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("attempt %s is not running; refusing to commit", plan.AttemptID)
		}
	}

	if err := setMetaTx(ctx, tx, "clock_step", fmt.Sprint(step)); err != nil {
		return nil, err
	}
	if err := setMetaTx(ctx, tx, "revision", fmt.Sprint(plan.BaseRevision+1)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// applyChangeTx validates one state change, records it, and mutates the
// canonical row. Unknown ids and bad operations abort the transaction.
func applyChangeTx(ctx context.Context, tx execer, eventID string, ch domain.Change) error {
	if !domain.ValidID(ch.ObjectID) {
		return fmt.Errorf("invalid object id %q", ch.ObjectID)
	}
	switch ch.Object {
	case "region":
		return applyRegionChange(ctx, tx, eventID, ch)
	case "entity":
		return applyEntityChange(ctx, tx, eventID, ch)
	case "relationship":
		return applyRelationshipChange(ctx, tx, eventID, ch)
	case "directive":
		return applyDirectiveChange(ctx, tx, eventID, ch)
	default:
		return fmt.Errorf("unknown object kind %q", ch.Object)
	}
}

func logStateChange(ctx context.Context, tx execer, eventID string, ch domain.Change, before any) error {
	var beforeArg any
	if before != nil {
		b, err := json.Marshal(before)
		if err != nil {
			return err
		}
		beforeArg = string(b)
	}
	patch, err := json.Marshal(ch.Patch)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO state_changes(event_id, object, object_id, op, patch, before) VALUES (?, ?, ?, ?, ?, ?)`,
		eventID, ch.Object, ch.ObjectID, ch.Op, string(patch), beforeArg)
	return err
}

func applyRegionChange(ctx context.Context, tx execer, eventID string, ch domain.Change) error {
	var attrs string
	var status string
	err := tx.QueryRowContext(ctx, `SELECT attrs, status FROM regions WHERE id = ?`, ch.ObjectID).Scan(&attrs, &status)
	if errors.Is(err, sql.ErrNoRows) {
		if ch.Op != "upsert" {
			return fmt.Errorf("region %s does not exist", ch.ObjectID)
		}
		name, _ := ch.Patch["name"].(string)
		if name == "" {
			return fmt.Errorf("region upsert %s needs a name", ch.ObjectID)
		}
		sub, _ := ch.Patch["attrs"].(map[string]any)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO regions(id, name, status, attrs) VALUES (?, ?, 'active', ?)`,
			ch.ObjectID, name, jsonOf(sub)); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, nil)
	}
	if err != nil {
		return err
	}
	switch ch.Op {
	case "set_attrs":
		merged := mergePatch(decodeAttrs(attrs), ch.Patch)
		if _, err := tx.ExecContext(ctx, `UPDATE regions SET attrs = ? WHERE id = ?`, jsonOf(merged), ch.ObjectID); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, decodeAttrs(attrs))
	case "set_status":
		v, _ := ch.Patch["status"].(string)
		if !domain.ValidStatus("region", v) {
			return fmt.Errorf("region %s: invalid status %q", ch.ObjectID, v)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE regions SET status = ? WHERE id = ?`, v, ch.ObjectID); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, map[string]any{"status": status})
	default:
		return fmt.Errorf("region %s: unsupported op %q", ch.ObjectID, ch.Op)
	}
}

func applyEntityChange(ctx context.Context, tx execer, eventID string, ch domain.Change) error {
	var kind, name, status, attrs string
	var region sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT kind, name, status, attrs, region_id FROM entities WHERE id = ?`, ch.ObjectID).Scan(&kind, &name, &status, &attrs, &region)
	if errors.Is(err, sql.ErrNoRows) {
		if ch.Op != "upsert" {
			return fmt.Errorf("entity %s does not exist", ch.ObjectID)
		}
		k, _ := ch.Patch["kind"].(string)
		n, _ := ch.Patch["name"].(string)
		if k == "" || n == "" {
			return fmt.Errorf("entity upsert %s needs kind and name", ch.ObjectID)
		}
		if k != "person" && k != "group" && k != "org" && k != "settlement" && k != "polity" {
			return fmt.Errorf("entity upsert %s: invalid kind %q", ch.ObjectID, k)
		}
		newRegion := nullable(stringPatch(ch.Patch, "region_id"))
		if rid := stringPatch(ch.Patch, "region_id"); rid != "" {
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM regions WHERE id = ?`, rid); err != nil {
				return fmt.Errorf("entity upsert %s: region %s: %w", ch.ObjectID, rid, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entities(id, kind, name, region_id, status, attrs) VALUES (?, ?, ?, ?, 'active', ?)`,
			ch.ObjectID, k, n, newRegion, jsonOf(attrsFromPatch(ch.Patch))); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, nil)
	}
	if err != nil {
		return err
	}
	switch ch.Op {
	case "set_attrs":
		if _, bad := ch.Patch["region_id"]; bad {
			// A region_id in attrs would pose as a location while the
			// canonical column stayed put; movement is upsert's job.
			return fmt.Errorf("entity %s: set_attrs may not set region_id; use upsert to move", ch.ObjectID)
		}
		merged := mergePatch(decodeAttrs(attrs), ch.Patch)
		if n, ok := ch.Patch["name"].(string); ok && n != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE entities SET attrs = ?, name = ? WHERE id = ?`, jsonOf(merged), n, ch.ObjectID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE entities SET attrs = ? WHERE id = ?`, jsonOf(merged), ch.ObjectID); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, decodeAttrs(attrs))
	case "set_status":
		v, _ := ch.Patch["status"].(string)
		if !domain.ValidStatus("entity", v) {
			return fmt.Errorf("entity %s: invalid status %q", ch.ObjectID, v)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entities SET status = ? WHERE id = ?`, v, ch.ObjectID); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, map[string]any{"status": status})
	case "upsert":
		// Existing entity: merge attrs (a minimal patch must not erase
		// established capabilities) and honor region_id as a move of
		// the canonical location column.
		merged := mergePatch(decodeAttrs(attrs), attrsFromPatch(ch.Patch))
		newAttrs := jsonOf(merged)
		newRegion := region // current location unless the patch moves it
		regionArg := any(nil)
		if rid := stringPatch(ch.Patch, "region_id"); rid != "" && rid != region.String {
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM regions WHERE id = ?`, rid); err != nil {
				return fmt.Errorf("entity move %s: destination region %s: %w", ch.ObjectID, rid, err)
			}
			newRegion = sql.NullString{String: rid, Valid: true}
			regionArg = rid
		}
		var nameArg any
		if n := stringPatch(ch.Patch, "name"); n != "" {
			nameArg = n
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entities SET attrs = ?, name = COALESCE(?, name), region_id = COALESCE(?, region_id) WHERE id = ?`,
			newAttrs, nameArg, regionArg, ch.ObjectID); err != nil {
			return err
		}
		// Audit before-image carries the old attrs and, on a move, the
		// old canonical location.
		before := decodeAttrs(attrs)
		if region.Valid && newRegion.String != region.String {
			before["region_id"] = region.String
		}
		return logStateChange(ctx, tx, eventID, ch, before)
	default:
		return fmt.Errorf("entity %s: unsupported op %q", ch.ObjectID, ch.Op)
	}
}

func applyRelationshipChange(ctx context.Context, tx execer, eventID string, ch domain.Change) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM relationships WHERE id = ?`, ch.ObjectID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		if ch.Op != "upsert" {
			return fmt.Errorf("relationship %s does not exist", ch.ObjectID)
		}
		src, _ := ch.Patch["source"].(string)
		dst, _ := ch.Patch["target"].(string)
		kind, _ := ch.Patch["kind"].(string)
		if src == "" || dst == "" || kind == "" || src == dst {
			return fmt.Errorf("relationship upsert %s needs distinct source, target, and kind", ch.ObjectID)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO relationships(id, source_id, target_id, kind, status, attrs) VALUES (?, ?, ?, ?, 'active', ?)`,
			ch.ObjectID, src, dst, kind, jsonOf(attrsFromPatch(ch.Patch))); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, nil)
	}
	if err != nil {
		return err
	}
	switch ch.Op {
	case "set_status":
		v, _ := ch.Patch["status"].(string)
		if !domain.ValidStatus("relationship", v) {
			return fmt.Errorf("relationship %s: invalid status %q", ch.ObjectID, v)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE relationships SET status = ? WHERE id = ?`, v, ch.ObjectID); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, map[string]any{"status": status})
	case "set_attrs":
		var attrs string
		if err := tx.QueryRowContext(ctx, `SELECT attrs FROM relationships WHERE id = ?`, ch.ObjectID).Scan(&attrs); err != nil {
			return err
		}
		merged := mergePatch(decodeAttrs(attrs), ch.Patch)
		if _, err := tx.ExecContext(ctx, `UPDATE relationships SET attrs = ? WHERE id = ?`, jsonOf(merged), ch.ObjectID); err != nil {
			return err
		}
		return logStateChange(ctx, tx, eventID, ch, decodeAttrs(attrs))
	default:
		return fmt.Errorf("relationship %s: unsupported op %q", ch.ObjectID, ch.Op)
	}
}

func applyDirectiveChange(ctx context.Context, tx execer, eventID string, ch domain.Change) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM directives WHERE id = ?`, ch.ObjectID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("directive %s does not exist", ch.ObjectID)
		}
		return err
	}
	if ch.Op != "set_status" {
		return fmt.Errorf("directive %s: unsupported op %q", ch.ObjectID, ch.Op)
	}
	v, _ := ch.Patch["status"].(string)
	if v != "pending" && v != "active" && v != "fulfilled" && v != "conflicting" {
		return fmt.Errorf("directive %s: invalid status %q", ch.ObjectID, v)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE directives SET status = ? WHERE id = ?`, v, ch.ObjectID); err != nil {
		return err
	}
	return logStateChange(ctx, tx, eventID, ch, map[string]any{"status": status})
}

func mergePatch(attrs, patch map[string]any) map[string]any {
	if attrs == nil {
		attrs = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(attrs, k)
		} else {
			attrs[k] = v
		}
	}
	return attrs
}

// attrsFromPatch extracts the attribute payload of an upsert patch.
// Control keys (kind, name, region_id, source, target) are not attributes.
func attrsFromPatch(patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range patch {
		switch k {
		case "kind", "name", "region_id", "source", "target":
			continue
		}
		out[k] = v
	}
	return out
}

func stringPatch(patch map[string]any, key string) string {
	s, _ := patch[key].(string)
	return s
}
