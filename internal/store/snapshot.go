package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/dnym/herodotos/internal/domain"
)

// Snapshot is an immutable view of the committed world at one step
// boundary, including reports delivered by that boundary.
type Snapshot struct {
	Meta             domain.Meta
	Regions          map[domain.ID]*domain.Region
	Entities         map[domain.ID]*domain.Entity
	Relationships    map[domain.ID]*domain.Relationship
	Connections      []domain.Connection
	Directives       map[domain.ID]*domain.Directive
	Memories         map[domain.ID]*domain.Memory
	DeliveredReports map[domain.ID][]domain.Report // by target region
	Beliefs          map[domain.ID][]domain.Belief
	WorldRefs        []domain.ReferenceDoc
	RegionRefs       map[domain.ID][]domain.ReferenceDoc
	RecentEvents     []domain.Event
}

// LoadSnapshot reads the committed state at the current clock step.
// Reports appear once their earliest delivery step has passed; no
// in-flight attempt can make them visible early.
func (d *DB) LoadSnapshot(ctx context.Context) (*Snapshot, error) {
	return d.loadSnapshot(ctx, 0)
}

// LoadSnapshotForAttempt reads the same state but applies the report
// deliveries due at the start of the given step: agents working on that
// step must see news arriving with it. The marking itself is only
// durable when the step commits (CommitStep), so a failed attempt
// leaves no trace and every retry of the same step sees identical
// knowledge.
func (d *DB) LoadSnapshotForAttempt(ctx context.Context, step int) (*Snapshot, error) {
	return d.loadSnapshot(ctx, step)
}

// qer is the subset of *sql.DB and *sql.Tx that snapshot reads need.
type qer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// closeRows closes result sets and reports the iteration error, if
// any: rows.Next() can stop early on a real error that only rows.Err()
// reveals.
func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// loadSnapshot reads the snapshot through one transaction so no commit
// can interleave and mix an old clock with new entities or events.
func (d *DB) loadSnapshot(ctx context.Context, attemptStep int) (*Snapshot, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	s, err := d.snapshotTx(ctx, tx, attemptStep)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

// snapshotTx reads the whole snapshot on the given executor.
func (d *DB) snapshotTx(ctx context.Context, tx *sql.Tx, attemptStep int) (*Snapshot, error) {
	meta, err := getMeta(ctx, tx)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{
		Meta:             meta,
		Regions:          map[domain.ID]*domain.Region{},
		Entities:         map[domain.ID]*domain.Entity{},
		Relationships:    map[domain.ID]*domain.Relationship{},
		Directives:       map[domain.ID]*domain.Directive{},
		Memories:         map[domain.ID]*domain.Memory{},
		DeliveredReports: map[domain.ID][]domain.Report{},
		Beliefs:          map[domain.ID][]domain.Belief{},
		RegionRefs:       map[domain.ID][]domain.ReferenceDoc{},
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, name, status, attrs FROM regions`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r domain.Region
		var attrs string
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &attrs); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		r.Attrs = decodeAttrs(attrs)
		s.Regions[r.ID] = &r
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT region_a, region_b, travel_days FROM connections ORDER BY region_a, region_b`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c domain.Connection
		if err := rows.Scan(&c.A, &c.B, &c.TravelDay); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		s.Connections = append(s.Connections, c)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT id, kind, name, COALESCE(region_id,''), status, attrs FROM entities`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e domain.Entity
		var attrs string
		if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &e.RegionID, &e.Status, &attrs); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		e.Attrs = decodeAttrs(attrs)
		s.Entities[e.ID] = &e
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT id, source_id, target_id, kind, status, attrs FROM relationships`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r domain.Relationship
		var attrs string
		if err := rows.Scan(&r.ID, &r.SourceID, &r.TargetID, &r.Kind, &r.Status, &attrs); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		r.Attrs = decodeAttrs(attrs)
		s.Relationships[r.ID] = &r
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT id, text, hard, status, COALESCE(notes,'') FROM directives`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var dd domain.Directive
		var hard int
		if err := rows.Scan(&dd.ID, &dd.Text, &hard, &dd.Status, &dd.Notes); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		dd.Hard = hard == 1
		s.Directives[dd.ID] = &dd
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT region_id, summary, open_threads, updated_step FROM memories`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m domain.Memory
		var threads string
		if err := rows.Scan(&m.RegionID, &m.Summary, &threads, &m.UpdatedStep); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		json.Unmarshal([]byte(threads), &m.OpenThreads)
		s.Memories[m.RegionID] = &m
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	horizon := meta.ClockStep
	if attemptStep > horizon {
		horizon = attemptStep
	}
	rows, err = tx.QueryContext(ctx,
		`SELECT id, event_id, target_region_id, earliest_step,
		        COALESCE(delivered_step, ?)
		 FROM reports
		 WHERE delivered_step IS NOT NULL OR earliest_step <= ?
		 ORDER BY COALESCE(delivered_step, ?), id`,
		horizon, horizon, horizon)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rep domain.Report
		if err := rows.Scan(&rep.ID, &rep.EventID, &rep.TargetRegionID, &rep.EarliestStep, &rep.DeliveredStep); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		s.DeliveredReports[rep.TargetRegionID] = append(s.DeliveredReports[rep.TargetRegionID], rep)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT id, region_id, COALESCE(subject_id,''), modality, statement, as_of_step FROM beliefs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b domain.Belief
		if err := rows.Scan(&b.ID, &b.RegionID, &b.SubjectID, &b.Modality, &b.Statement, &b.AsOfStep); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		s.Beliefs[b.RegionID] = append(s.Beliefs[b.RegionID], b)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT id, scope, COALESCE(region_id,''), binding, title, body, keywords,
		        COALESCE(description,''), enabled
		 FROM reference_docs WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref domain.ReferenceDoc
		var binding, enabled int
		if err := rows.Scan(&ref.ID, &ref.Scope, &ref.RegionID, &binding, &ref.Title,
			&ref.Body, &ref.Keywords, &ref.Description, &enabled); err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		ref.Binding = binding == 1
		ref.Enabled = enabled == 1
		if ref.Scope == "world" {
			s.WorldRefs = append(s.WorldRefs, ref)
		} else {
			s.RegionRefs[ref.RegionID] = append(s.RegionRefs[ref.RegionID], ref)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx,
		`SELECT id, step, type, title, prose, region_ids, entity_ids, cause_ids
		 FROM events WHERE step > ? ORDER BY step, id`, meta.ClockStep-8)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			if err := closeRows(rows); err != nil {
				return nil, err
			}
			return nil, err
		}
		s.RecentEvents = append(s.RecentEvents, ev)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	return s, nil
}

// BeginAttempt creates the diagnostics row for a new step attempt.
// Report deliveries are deliberately not marked here: delivery markers
// are written by CommitStep in the step's transaction, so a failed
// attempt leaves report rows untouched and every retry of the same
// step sees identical report eligibility (eligibility itself is purely
// time-based, computed in LoadSnapshot).
func (d *DB) BeginAttempt(ctx context.Context, step int) (string, error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	id, err := nextIDTx(ctx, tx, "att")
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempts(id, step, status, started_utc) VALUES (?, ?, 'running', ?)`,
		id, step, utcNow()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// MarkAttemptFailed records a non-committed attempt's outcome.
func (d *DB) MarkAttemptFailed(ctx context.Context, attemptID, reason string) error {
	_, err := d.sqlDB.ExecContext(ctx,
		`UPDATE attempts SET status='failed', ended_utc=?, error=? WHERE id=? AND status='running'`,
		utcNow(), reason, attemptID)
	return err
}

// RecordProposal stores an agent proposal for inspection. Proposals are
// attempt diagnostics; they never become canonical on their own.
func (d *DB) RecordProposal(ctx context.Context, attemptID string, p domain.Proposal) error {
	changes, _ := json.Marshal(eventChanges(p.Events))
	regions, _ := json.Marshal(eventRegions(p.Events))
	entities, _ := json.Marshal(eventEntities(p.Events))
	causes, _ := json.Marshal(eventCauses(p.Events))
	delivery, _ := json.Marshal(eventDelivery(p.Events))
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO proposals(id, attempt_id, author, kind, prose, changes, affects_regions, affects_entities, causes, delivery)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, attemptID, p.Author, p.Kind, p.Prose, string(changes), string(regions),
		string(entities), string(causes), string(delivery))
	return err
}

// RecordModelCall stores one inference interaction for auditability.
func (d *DB) RecordModelCall(ctx context.Context, attemptID, role, model, promptSHA string, reqJSON, respJSON string, inTok, outTok, durMS int, callErr string) error {
	var errArg any
	if callErr != "" {
		errArg = callErr
	}
	_, err := d.sqlDB.ExecContext(ctx,
		`INSERT INTO model_calls(attempt_id, role, model, prompt_sha256, request_json, response_json, input_tokens, output_tokens, duration_ms, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attemptID, role, model, promptSHA, reqJSON, respJSON, inTok, outTok, durMS, errArg)
	return err
}

func scanEvent(s interface{ Scan(dest ...any) error }) (domain.Event, error) {
	var ev domain.Event
	var regions, entities, causes string
	if err := s.Scan(&ev.ID, &ev.Step, &ev.Type, &ev.Title, &ev.Prose, &regions, &entities, &causes); err != nil {
		return domain.Event{}, err
	}
	json.Unmarshal([]byte(regions), &ev.RegionIDs)
	json.Unmarshal([]byte(entities), &ev.EntityIDs)
	json.Unmarshal([]byte(causes), &ev.CauseIDs)
	return ev, nil
}

func decodeAttrs(s string) map[string]any {
	m := map[string]any{}
	if s != "" {
		json.Unmarshal([]byte(s), &m)
	}
	return m
}

func jsonIDList(ids []domain.ID) string {
	if ids == nil {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}
