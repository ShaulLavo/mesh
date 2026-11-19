package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestCommitAfterCancellationReportsTheCancellation(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// Finish the transaction as database/sql's cancellation rollback would, so
	// Commit takes the ErrTxDone path deterministically.
	_ = tx.Rollback()
	if err := commitTx(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled commit = %v, want context.Canceled", err)
	}
}

func TestCommitAfterRollbackStillReportsTxDone(t *testing.T) {
	store := openTestStore(t)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := commitTx(t.Context(), tx); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("commit after rollback = %v, want sql.ErrTxDone", err)
	}
}
