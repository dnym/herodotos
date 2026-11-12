package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dnym/herodotos/internal/domain"
)

// WorldSpec is the on-disk description of an initial world. Unknown
// details stay absent; only validated proposals may invent consequential
// facts later.
type WorldSpec struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	StartDate string `json:"start_date"`
	StepDays  int    `json:"step_days"`
	Regions   []struct {
		ID    domain.ID      `json:"id"`
		Name  string         `json:"name"`
		Attrs map[string]any `json:"attrs,omitempty"`
	} `json:"regions"`
	Connections []struct {
		A         domain.ID `json:"a"`
		B         domain.ID `json:"b"`
		TravelDay int       `json:"travel_days"`
	} `json:"connections"`
	Entities []struct {
		ID       domain.ID      `json:"id"`
		Kind     string         `json:"kind"`
		Name     string         `json:"name"`
		RegionID domain.ID      `json:"region_id,omitempty"`
		Attrs    map[string]any `json:"attrs,omitempty"`
	} `json:"entities,omitempty"`
	Relationships []struct {
		ID     domain.ID      `json:"id"`
		Source domain.ID      `json:"source"`
		Target domain.ID      `json:"target"`
		Kind   string         `json:"kind"`
		Attrs  map[string]any `json:"attrs,omitempty"`
	} `json:"relationships,omitempty"`
	Directives []struct {
		ID   domain.ID `json:"id,omitempty"`
		Text string    `json:"text"`
		Hard bool      `json:"hard,omitempty"`
	} `json:"directives,omitempty"`
	ReferenceDocs []struct {
		ID          domain.ID `json:"id"`
		Scope       string    `json:"scope"`
		RegionID    string    `json:"region_id,omitempty"`
		Binding     bool      `json:"binding,omitempty"`
		Title       string    `json:"title"`
		Description string    `json:"description,omitempty"`
		Body        string    `json:"body"`
		Keywords    string    `json:"keywords,omitempty"`
	} `json:"reference_docs,omitempty"`
	Memories map[domain.ID]struct {
		Summary     string   `json:"summary"`
		OpenThreads []string `json:"open_threads,omitempty"`
	} `json:"memories,omitempty"`
}

// LoadWorldSpec reads and validates a world JSON file.
func LoadWorldSpec(path string) (*WorldSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var w WorldSpec
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("parse world file: %w", err)
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

func (w *WorldSpec) validate() error {
	if w.Name == "" {
		return fmt.Errorf("world: name is required")
	}
	if _, err := time.Parse("2006-01-02", w.StartDate); err != nil {
		return fmt.Errorf("world: invalid start_date %q", w.StartDate)
	}
	if w.StepDays < 1 {
		w.StepDays = 7
	}
	if len(w.Regions) < 2 {
		return fmt.Errorf("world: at least two regions are required")
	}
	seen := map[domain.ID]bool{}
	for _, r := range w.Regions {
		if !domain.ValidID(r.ID) {
			return fmt.Errorf("world: invalid region id %q", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("world: duplicate region id %q", r.ID)
		}
		seen[r.ID] = true
	}
	for _, c := range w.Connections {
		if !seen[c.A] || !seen[c.B] || c.A == c.B {
			return fmt.Errorf("world: connection %q-%q references unknown region", c.A, c.B)
		}
		if c.TravelDay < 1 {
			return fmt.Errorf("world: connection %q-%q needs travel_days >= 1", c.A, c.B)
		}
	}
	for _, e := range w.Entities {
		if !domain.ValidID(e.ID) {
			return fmt.Errorf("world: invalid entity id %q", e.ID)
		}
		if seen[e.ID] {
			return fmt.Errorf("world: duplicate id %q", e.ID)
		}
		seen[e.ID] = true
		switch e.Kind {
		case "person", "group", "org", "settlement", "polity":
		default:
			return fmt.Errorf("world: entity %q has invalid kind %q", e.ID, e.Kind)
		}
		if e.RegionID != "" && !seen[e.RegionID] {
			return fmt.Errorf("world: entity %q references unknown region %q", e.ID, e.RegionID)
		}
	}
	for _, r := range w.Relationships {
		if !domain.ValidID(r.ID) || !seen[r.Source] || !seen[r.Target] || r.Source == r.Target {
			return fmt.Errorf("world: relationship %q has bad endpoints", r.ID)
		}
	}
	for _, ref := range w.ReferenceDocs {
		if !domain.ValidID(ref.ID) {
			return fmt.Errorf("world: invalid reference id %q", ref.ID)
		}
		if ref.Scope == "region" && !seen[ref.RegionID] {
			return fmt.Errorf("world: reference %q references unknown region %q", ref.ID, ref.RegionID)
		}
	}
	return nil
}

// InitWorld writes the initial world in a single transaction. It fails if
// the database already contains a world.
func (d *DB) InitWorld(ctx context.Context, w *WorldSpec) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var worlds int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key='world_id'`).Scan(&worlds); err != nil {
		return err
	}
	if worlds > 0 {
		return fmt.Errorf("database %s already contains a world", d.Path)
	}

	id := w.ID
	if id == "" {
		id = "world-1"
	}
	for _, kv := range [][2]string{
		{"world_id", id},
		{"world_name", w.Name},
		{"start_date", w.StartDate},
		{"step_days", fmt.Sprint(w.StepDays)},
		{"clock_step", "0"},
		{"revision", "0"},
		{"next-evt", "0"},
		{"next-rep", "0"},
		{"next-bel", "0"},
		{"next-att", "0"},
		{"next-prop", "0"},
		{"next-dir", "0"},
		{"next-rel", "0"},
	} {
		if err := setMetaTx(ctx, tx, kv[0], kv[1]); err != nil {
			return err
		}
	}

	for _, r := range w.Regions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO regions(id, name, status, attrs) VALUES (?, ?, 'active', ?)`,
			r.ID, r.Name, jsonOf(r.Attrs)); err != nil {
			return err
		}
	}
	for _, c := range w.Connections {
		a, b := c.A, c.B
		if a > b {
			a, b = b, a
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO connections(region_a, region_b, travel_days) VALUES (?, ?, ?)`,
			a, b, c.TravelDay); err != nil {
			return err
		}
	}
	for _, e := range w.Entities {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entities(id, kind, name, region_id, status, attrs) VALUES (?, ?, ?, ?, 'active', ?)`,
			e.ID, e.Kind, e.Name, nullable(e.RegionID), jsonOf(e.Attrs)); err != nil {
			return err
		}
	}
	for _, r := range w.Relationships {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO relationships(id, source_id, target_id, kind, status, attrs) VALUES (?, ?, ?, ?, 'active', ?)`,
			r.ID, r.Source, r.Target, r.Kind, jsonOf(r.Attrs)); err != nil {
			return err
		}
	}
	for _, dir := range w.Directives {
		id := dir.ID
		if id == "" {
			var err error
			if id, err = nextIDTx(ctx, tx, "dir"); err != nil {
				return err
			}
		}
		hard := 0
		if dir.Hard {
			hard = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO directives(id, text, hard, status) VALUES (?, ?, ?, 'pending')`,
			id, dir.Text, hard); err != nil {
			return err
		}
	}
	for _, ref := range w.ReferenceDocs {
		binding := 0
		if ref.Binding {
			binding = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reference_docs(id, scope, region_id, binding, title, body, keywords, description)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ref.ID, ref.Scope, nullable(ref.RegionID), binding, ref.Title, ref.Body, ref.Keywords, ref.Description); err != nil {
			return err
		}
	}
	for rid, mem := range w.Memories {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memories(region_id, summary, open_threads, updated_step) VALUES (?, ?, ?, 0)`,
			rid, mem.Summary, jsonStrings(mem.OpenThreads)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func jsonOf(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func jsonStrings(v []string) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
