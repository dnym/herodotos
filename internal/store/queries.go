package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dnym/herodotos/internal/domain"
)

// EventQuery filters the canonical timeline.
type EventQuery struct {
	RegionID string // empty for all regions
	Limit    int
	Offset   int
}

// ListEvents returns committed events, newest first.
func (d *DB) ListEvents(ctx context.Context, q EventQuery) ([]domain.Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, step, type, title, prose, region_ids, entity_ids, cause_ids FROM events WHERE 1=1`
	var args []any
	if q.RegionID != "" {
		query += ` AND EXISTS (SELECT 1 FROM json_each(region_ids) WHERE value = ?)`
		args = append(args, q.RegionID)
	}
	query += ` ORDER BY step DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, q.Offset)
	rows, err := d.sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetEvent loads one committed event.
func (d *DB) GetEvent(ctx context.Context, id domain.ID) (domain.Event, error) {
	row := d.sqlDB.QueryRowContext(ctx,
		`SELECT id, step, type, title, prose, region_ids, entity_ids, cause_ids FROM events WHERE id = ?`, id)
	return scanEvent(row)
}

// ListRegions returns all regions in stable order.
func (d *DB) ListRegions(ctx context.Context) ([]domain.Region, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, name, status, attrs FROM regions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Region
	for rows.Next() {
		var r domain.Region
		var attrs string
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &attrs); err != nil {
			return nil, err
		}
		r.Attrs = decodeAttrs(attrs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEntities returns entities, optionally limited to one region.
func (d *DB) ListEntities(ctx context.Context, regionID string) ([]domain.Entity, error) {
	query := `SELECT id, kind, name, COALESCE(region_id,''), status, attrs FROM entities`
	var args []any
	if regionID != "" {
		query += ` WHERE region_id = ?`
		args = append(args, regionID)
	}
	query += ` ORDER BY id`
	rows, err := d.sqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Entity
	for rows.Next() {
		var e domain.Entity
		var attrs string
		if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &e.RegionID, &e.Status, &attrs); err != nil {
			return nil, err
		}
		e.Attrs = decodeAttrs(attrs)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListDirectives returns all directives in stable order.
func (d *DB) ListDirectives(ctx context.Context) ([]domain.Directive, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, text, hard, status, COALESCE(notes,'') FROM directives ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Directive
	for rows.Next() {
		var dd domain.Directive
		var hard int
		if err := rows.Scan(&dd.ID, &dd.Text, &hard, &dd.Status, &dd.Notes); err != nil {
			return nil, err
		}
		dd.Hard = hard == 1
		out = append(out, dd)
	}
	return out, rows.Err()
}

// UpdateDirectiveText rewrites a directive's wording. Directive text
// belongs to the creative director, never to agents; this is the human
// path. Rewording resets the assessment status: an old verdict does not
// describe the new text. The write and the creative-input revision bump
// share one transaction, so any step that snapshotted the old rules
// fails its commit check instead of adjudicating against changed ones.
// Returns false when the id does not exist.
func (d *DB) UpdateDirectiveText(ctx context.Context, id domain.ID, text string) (bool, error) {
	return d.mutateWithRevisionBump(ctx, func(tx *sql.Tx) (bool, error) {
		res, err := tx.ExecContext(ctx,
			`UPDATE directives SET text = ?, status = 'pending' WHERE id = ?`, text, id)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		return n == 1, err
	})
}

// CreateDirective adds a new active directive and returns its id.
func (d *DB) CreateDirective(ctx context.Context, text string, hard bool) (string, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := nextIDTx(ctx, tx, "dir")
	if err != nil {
		return "", err
	}
	h := 0
	if hard {
		h = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO directives(id, text, hard, status) VALUES (?, ?, ?, 'active')`, id, text, h); err != nil {
		return "", err
	}
	if err := bumpRevisionTx(ctx, tx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// mutateWithRevisionBump runs one human creative-input mutation and the
// creative-input revision bump inside a single transaction, so a step
// that loaded its snapshot before the mutation fails CommitStep's
// revision check rather than committing an outcome adjudicated under
// the previous rules.
func (d *DB) mutateWithRevisionBump(ctx context.Context, mutate func(tx *sql.Tx) (bool, error)) (bool, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ok, err := mutate(tx)
	if err != nil || !ok {
		return false, err
	}
	if err := bumpRevisionTx(ctx, tx); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// bumpRevisionTx increments the commit revision counter, invalidating
// any step that snapshotted the world earlier.
func bumpRevisionTx(ctx context.Context, tx *sql.Tx) error {
	cur, err := currentRevisionTx(ctx, tx)
	if err != nil {
		return err
	}
	return setMetaTx(ctx, tx, "revision", fmt.Sprint(cur+1))
}

func currentRevisionTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'revision'`).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// ReportRow pairs a report with its event title for inspection.
type ReportRow struct {
	Report     domain.Report
	EventTitle string
}

// StateChangeRow is one structured state transition authorized by an event.
type StateChangeRow struct {
	Object   string
	ObjectID string
	Op       string
	Patch    string
	Before   string
}

// ListStateChanges returns the canonical changes an event applied, in the
// order they were committed.
func (d *DB) ListStateChanges(ctx context.Context, eventID domain.ID) ([]StateChangeRow, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT object, object_id, op, patch, COALESCE(before,'') FROM state_changes WHERE event_id = ? ORDER BY id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateChangeRow
	for rows.Next() {
		var c StateChangeRow
		if err := rows.Scan(&c.Object, &c.ObjectID, &c.Op, &c.Patch, &c.Before); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListReports returns all knowledge deliveries, pending and delivered.
func (d *DB) ListReports(ctx context.Context) ([]ReportRow, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT r.id, r.event_id, e.title, r.target_region_id, r.earliest_step, COALESCE(r.delivered_step, 0)
		 FROM reports r JOIN events e ON e.id = r.event_id
		 ORDER BY r.earliest_step, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportRow
	for rows.Next() {
		var rr ReportRow
		if err := rows.Scan(&rr.Report.ID, &rr.Report.EventID, &rr.EventTitle,
			&rr.Report.TargetRegionID, &rr.Report.EarliestStep, &rr.Report.DeliveredStep); err != nil {
			return nil, err
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

// ListAttempts returns recent attempts, newest first.
func (d *DB) ListAttempts(ctx context.Context, limit int) ([]domain.Attempt, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, step, status, COALESCE(error,''), COALESCE(pause_reason,'') FROM attempts ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		var a domain.Attempt
		if err := rows.Scan(&a.ID, &a.Step, &a.Status, &a.Error, &a.PauseReason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProposalRow holds a proposal with its structured fields as raw JSON for
// faithful inspection.
type ProposalRow struct {
	ID              string
	AttemptID       string
	Author          string
	Kind            string
	Prose           string
	Changes         string
	AffectsRegions  string
	AffectsEntities string
	Causes          string
	Delivery        string
}

// ListProposals returns the proposals recorded for one attempt.
func (d *DB) ListProposals(ctx context.Context, attemptID string) ([]ProposalRow, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT id, attempt_id, author, kind, prose,
		        COALESCE(changes,'[]'), COALESCE(affects_regions,'[]'),
		        COALESCE(affects_entities,'[]'), COALESCE(causes,'[]'), COALESCE(delivery,'{}')
		 FROM proposals WHERE attempt_id = ? ORDER BY id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalRow
	for rows.Next() {
		var p ProposalRow
		if err := rows.Scan(&p.ID, &p.AttemptID, &p.Author, &p.Kind, &p.Prose,
			&p.Changes, &p.AffectsRegions, &p.AffectsEntities, &p.Causes, &p.Delivery); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ModelCallRow describes one recorded inference call.
type ModelCallRow struct {
	Seq        int64
	Role       string
	Model      string
	PromptSHA  string
	InTokens   int
	OutTokens  int
	DurationMS int
	Error      string
	Response   string // raw reply text, for live inspection
}

// ListModelCalls returns the inference calls of one attempt.
func (d *DB) ListModelCalls(ctx context.Context, attemptID string) ([]ModelCallRow, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT seq, role, model, prompt_sha256, COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		        COALESCE(duration_ms,0), COALESCE(error,''), COALESCE(response_json,'')
		 FROM model_calls WHERE attempt_id = ? ORDER BY seq`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCallRow
	for rows.Next() {
		var m ModelCallRow
		if err := rows.Scan(&m.Seq, &m.Role, &m.Model, &m.PromptSHA,
			&m.InTokens, &m.OutTokens, &m.DurationMS, &m.Error, &m.Response); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetAttempt loads one attempt's summary row.
func (d *DB) GetAttempt(ctx context.Context, id domain.ID) (domain.Attempt, error) {
	var a domain.Attempt
	row := d.sqlDB.QueryRowContext(ctx,
		`SELECT id, step, status, COALESCE(error,''), COALESCE(pause_reason,'') FROM attempts WHERE id = ?`, id)
	if err := row.Scan(&a.ID, &a.Step, &a.Status, &a.Error, &a.PauseReason); err != nil {
		return a, err
	}
	return a, nil
}

// RunningAttempt returns the attempt of a step in flight, if any. The
// live viewer points the browser at it while a run is active.
func (d *DB) RunningAttempt(ctx context.Context) (string, bool, error) {
	var id string
	err := d.sqlDB.QueryRowContext(ctx,
		`SELECT id FROM attempts WHERE status = 'running' ORDER BY id DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// --- proposal denormalization helpers used by RecordProposal ---

func eventChanges(events []domain.PropEvent) []domain.Change {
	var out []domain.Change
	for _, ev := range events {
		out = append(out, ev.Changes...)
	}
	return out
}

func eventRegions(events []domain.PropEvent) []domain.ID {
	seen := map[domain.ID]bool{}
	var out []domain.ID
	add := func(id domain.ID) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, ev := range events {
		for _, ch := range ev.Changes {
			if ch.Object == "region" {
				add(ch.ObjectID)
			}
		}
		for r := range ev.Delivery {
			add(r)
		}
	}
	return out
}

func eventEntities(events []domain.PropEvent) []domain.ID {
	seen := map[domain.ID]bool{}
	var out []domain.ID
	add := func(id domain.ID) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, ev := range events {
		for _, id := range ev.AffectsEntities {
			add(id)
		}
		for _, ch := range ev.Changes {
			if ch.Object == "entity" {
				add(ch.ObjectID)
			}
		}
	}
	return out
}

func eventCauses(events []domain.PropEvent) []domain.ID {
	seen := map[domain.ID]bool{}
	var out []domain.ID
	for _, ev := range events {
		for _, id := range ev.Causes {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func eventDelivery(events []domain.PropEvent) map[string]int {
	out := map[string]int{}
	for _, ev := range events {
		for r, step := range ev.Delivery {
			if cur, ok := out[r]; !ok || step < cur {
				out[r] = step
			}
		}
	}
	return out
}
