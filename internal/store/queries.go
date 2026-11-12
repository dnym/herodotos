package store

import (
	"context"

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

// ReportRow pairs a report with its event title for inspection.
type ReportRow struct {
	Report     domain.Report
	EventTitle string
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
		`SELECT id, step, status, COALESCE(error,'') FROM attempts ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		var a domain.Attempt
		if err := rows.Scan(&a.ID, &a.Step, &a.Status, &a.Error); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProposalRow holds a proposal with its structured fields as raw JSON for
// faithful inspection.
type ProposalRow struct {
	ID               string
	AttemptID        string
	Author           string
	Kind             string
	Prose            string
	Changes          string
	AffectsRegions   string
	AffectsEntities  string
	Causes           string
	Delivery         string
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
	Seq           int64
	Role          string
	Model         string
	PromptSHA     string
	InTokens      int
	OutTokens     int
	DurationMS    int
	Error         string
}

// ListModelCalls returns the inference calls of one attempt.
func (d *DB) ListModelCalls(ctx context.Context, attemptID string) ([]ModelCallRow, error) {
	rows, err := d.sqlDB.QueryContext(ctx,
		`SELECT seq, role, model, prompt_sha256, COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		        COALESCE(duration_ms,0), COALESCE(error,'')
		 FROM model_calls WHERE attempt_id = ? ORDER BY seq`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCallRow
	for rows.Next() {
		var m ModelCallRow
		if err := rows.Scan(&m.Seq, &m.Role, &m.Model, &m.PromptSHA,
			&m.InTokens, &m.OutTokens, &m.DurationMS, &m.Error); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
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
