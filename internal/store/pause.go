package store

import (
	"context"
	"database/sql"
	"errors"
)

// worldPausedKey marks a durably halted world: while set, the engine
// refuses new steps until ResumeWorld clears it. The flag survives process
// restarts; the attempt row keeps the diagnostic explanation.
const worldPausedKey = "world_paused"

// PauseWorld halts the world and explains why. attemptID (may be empty)
// records the reason on the attempt that triggered the pause.
func (d *DB) PauseWorld(ctx context.Context, attemptID, reason string) error {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if attemptID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE attempts SET pause_reason = ? WHERE id = ?`, reason, attemptID); err != nil {
			return err
		}
	}
	if err := setMetaTx(ctx, tx, worldPausedKey, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// WorldPause reports the reason the world is paused, and whether it is.
func (d *DB) WorldPause(ctx context.Context) (string, bool, error) {
	return worldPauseTx(ctx, d.sqlDB)
}

func worldPauseTx(ctx context.Context, db execer) (string, bool, error) {
	var reason string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, worldPausedKey).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return reason, true, nil
}

// ResumeWorld clears a durable pause so stepping can continue.
func (d *DB) ResumeWorld(ctx context.Context) error {
	_, err := d.sqlDB.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, worldPausedKey)
	return err
}
