package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/dnym/herodotos/internal/domain"
)

// CreateReference inserts one human-authored reference document,
// allocating a "ref-NNNNNN" id when ref.ID is empty. References are
// creative-direction material: only humans create or edit them.
func (d *DB) CreateReference(ctx context.Context, ref domain.ReferenceDoc) (string, error) {
	if err := validateRef(ref); err != nil {
		return "", err
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := ref.ID
	if id == "" {
		id, err = nextIDTx(ctx, tx, "ref")
		if err != nil {
			return "", err
		}
	}
	var region any
	if ref.Scope == "region" {
		region = ref.RegionID
	}
	binding := 0
	if ref.Binding {
		binding = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO reference_docs(id, scope, region_id, binding, title, body, keywords, description, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		id, ref.Scope, region, binding, ref.Title, ref.Body, ref.Keywords, ref.Description); err != nil {
		return "", fmt.Errorf("reference %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// UpdateReference rewrites an existing reference document, enabled flag
// included. It returns false when the id does not exist.
func (d *DB) UpdateReference(ctx context.Context, ref domain.ReferenceDoc) (bool, error) {
	if err := validateRef(ref); err != nil {
		return false, err
	}
	binding, enabled := 0, 0
	if ref.Binding {
		binding = 1
	}
	if ref.Enabled {
		enabled = 1
	}
	var region any
	if ref.Scope == "region" {
		region = ref.RegionID
	}
	res, err := d.sqlDB.ExecContext(ctx,
		`UPDATE reference_docs SET scope=?, region_id=?, binding=?, title=?, body=?, keywords=?, description=?, enabled=?
		 WHERE id = ?`,
		ref.Scope, region, binding, ref.Title, ref.Body, ref.Keywords, ref.Description, enabled, ref.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteReference removes a reference document.
func (d *DB) DeleteReference(ctx context.Context, id domain.ID) (bool, error) {
	res, err := d.sqlDB.ExecContext(ctx, `DELETE FROM reference_docs WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func validateRef(ref domain.ReferenceDoc) error {
	switch ref.Scope {
	case "world":
	case "region":
		if !domain.ValidID(ref.RegionID) {
			return fmt.Errorf("region-scoped reference needs a valid region id, got %q", ref.RegionID)
		}
	default:
		return fmt.Errorf("scope must be world or region, got %q", ref.Scope)
	}
	if strings.TrimSpace(ref.Title) == "" {
		return fmt.Errorf("reference needs a title")
	}
	if strings.TrimSpace(ref.Body) == "" {
		return fmt.Errorf("reference needs a body")
	}
	if len(ref.Body) > 40_000 {
		return fmt.Errorf("reference body too large (%d characters, max 40000)", len(ref.Body))
	}
	return nil
}

// ParseRefMarkdown reads a reference document file. Optional front
// matter between --- lines carries id, title, description, scope,
// region, binding, and keywords; everything after is the body. A title
// falls back to the first "# " heading, then to "untitled".
func ParseRefMarkdown(text string) (domain.ReferenceDoc, error) {
	var ref domain.ReferenceDoc
	body := strings.TrimSpace(text)
	if strings.HasPrefix(body, "---\n") {
		rest := body[4:]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return ref, fmt.Errorf("front matter is not closed")
		}
		for _, line := range strings.Split(rest[:end], "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				return ref, fmt.Errorf("front matter line %q is not key: value", line)
			}
			val = strings.TrimSpace(val)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "id":
				ref.ID = val
			case "title":
				ref.Title = val
			case "description":
				ref.Description = val
			case "scope":
				ref.Scope = strings.ToLower(val)
			case "region":
				ref.RegionID = val
			case "binding":
				ref.Binding = val == "true" || val == "yes" || val == "1"
			case "keywords":
				ref.Keywords = val
			default:
				return ref, fmt.Errorf("unknown front matter key %q", key)
			}
		}
		body = strings.TrimSpace(rest[end+len("\n---"):])
		if b := strings.Index(body, "\n"); strings.HasPrefix(body, "---") && b >= 0 {
			body = strings.TrimSpace(body[b:])
		}
	}
	if ref.Scope == "" {
		ref.Scope = "world"
		if ref.RegionID != "" {
			ref.Scope = "region"
		}
	}
	if ref.Title == "" {
		for _, line := range strings.SplitN(body, "\n", 3) {
			if t := strings.TrimSpace(strings.TrimPrefix(line, "#")); t != "" {
				ref.Title = t
				break
			}
		}
	}
	if ref.Title == "" {
		ref.Title = "untitled"
	}
	ref.Body = body
	return ref, validateRef(ref)
}
