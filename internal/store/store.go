// Package store persists canonical world state, committed history, and
// attempt diagnostics in SQLite. Structural invariants are enforced here
// with constraints and transactions.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/dnym/herodotos/internal/domain"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrLocked means another process holds the single-writer lock.
var ErrLocked = errors.New("database is already open for writing elsewhere")

// ErrStaleRevision means the world changed since the caller's snapshot.
var ErrStaleRevision = errors.New("world state changed; step not committed")

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is an opened Herodotos database with exclusive writer rights.
type DB struct {
	sqlDB    *sql.DB
	lockFile *os.File
	Path     string
}

// Open locks the database file, opens it with the required pragmas, runs
// migrations, and marks abandoned attempts from previous runs.
func Open(path string) (*DB, error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("%w: %s", ErrLocked, path)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(3000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		lf.Close()
		return nil, err
	}
	// One connection: the single writer serializes all access.
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		lf.Close()
		return nil, err
	}
	d := &DB{sqlDB: sqlDB, lockFile: lf, Path: path}
	if err := d.migrate(context.Background()); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := sqlDB.ExecContext(context.Background(),
		`UPDATE attempts SET status='interrupted', ended_utc=? WHERE status='running'`,
		utcNow()); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the database and writer lock.
func (d *DB) Close() error {
	err := d.sqlDB.Close()
	syscall.Flock(int(d.lockFile.Fd()), syscall.LOCK_UN)
	d.lockFile.Close()
	return err
}

// Begin starts a transaction. With _txlock=immediate the write lock is
// taken at BEGIN.
func (d *DB) Begin(ctx context.Context) (*sql.Tx, error) {
	return d.sqlDB.BeginTx(ctx, nil)
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sqlDB.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT PRIMARY KEY,
			applied_utc TEXT NOT NULL
		)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var exists int
		if err := d.sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE version = ?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := d.sqlDB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_utc) VALUES (?, ?)`, name, utcNow()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- meta ---

// GetMeta loads world metadata and the clock.
func (d *DB) GetMeta(ctx context.Context) (domain.Meta, error) {
	return getMeta(ctx, d.sqlDB)
}

func getMeta(ctx context.Context, ex execer) (domain.Meta, error) {
	rows, err := ex.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return domain.Meta{}, err
	}
	defer rows.Close()
	vals := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return domain.Meta{}, err
		}
		vals[k] = v
	}
	if vals["world_id"] == "" {
		return domain.Meta{}, errors.New("database is not initialized")
	}
	m := domain.Meta{WorldID: vals["world_id"], WorldName: vals["world_name"], StartDate: vals["start_date"]}
	fmt.Sscan(vals["step_days"], &m.StepDays)
	fmt.Sscan(vals["clock_step"], &m.ClockStep)
	fmt.Sscan(vals["revision"], &m.Revision)
	return m, nil
}

func setMetaTx(ctx context.Context, tx execer, key, value string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// idTables maps each generated identifier prefix to the table whose
// primary key it fills.
var idTables = map[string]string{
	"evt": "events",
	"rep": "reports",
	"bel": "beliefs",
	"att": "attempts",
	"dir": "directives",
	"ref": "reference_docs",
}

// nextIDTx allocates the next unused stable identifier of the given
// type. The sequence is a hint, not the authority: imported worlds and
// explicit ids may already occupy the generated namespace (a reference
// created as "ref-000001"), so the candidate is checked against its
// table and skipped while taken. Without that check one custom id
// would make every later automatic allocation collide forever.
func nextIDTx(ctx context.Context, tx execer, prefix string) (string, error) {
	table, ok := idTables[prefix]
	if !ok {
		return "", fmt.Errorf("no table registered for id prefix %q", prefix)
	}
	key := "next-" + prefix
	var cur string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		cur = "0"
	} else if err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscan(cur, &n); err != nil {
		return "", fmt.Errorf("sequence %s corrupted: %w", key, err)
	}
	for {
		n++
		id := fmt.Sprintf("%s-%06d", prefix, n)
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE id = ?`, id).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			if err := setMetaTx(ctx, tx, key, fmt.Sprint(n)); err != nil {
				return "", err
			}
			return id, nil
		}
	}
}

func utcNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}
