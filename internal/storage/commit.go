package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// commitTx commits tx, which was begun with ctx. database/sql rolls a
// transaction back as soon as its context ends, so a commit racing a
// cancellation reports sql.ErrTxDone; callers need the cancellation instead.
func commitTx(ctx context.Context, tx *sql.Tx) error {
	err := tx.Commit()
	if errors.Is(err, sql.ErrTxDone) && ctx.Err() != nil {
		return fmt.Errorf("transaction context ended: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
