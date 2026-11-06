package updateinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestGrantSettlesVisibleJournalBeforeAcknowledging(t *testing.T) {
	f := newFixture(t)
	f.stage()
	syncReal := syncDirectory
	t.Cleanup(func() { syncDirectory = syncReal })
	fault := errors.New("injected journal directory barrier failure")
	calls := 0
	syncDirectory = func(path string) error {
		if path != transactionDir(f.engine.cfg.StateDir) {
			return syncReal(path)
		}
		data, readErr := os.ReadFile(journalPath(f.engine.cfg.StateDir))
		if readErr != nil {
			return fmt.Errorf("read journal at sync barrier: %w", readErr)
		}
		if bytes.Contains(data, []byte(`"phase": "granted"`)) {
			calls++
			return fault
		}
		return syncReal(path)
	}
	status, err := f.engine.Grant(context.Background(), f.request.ID, 1)
	if !errors.Is(err, fault) || status.Phase != Granted {
		t.Fatalf("initial grant = %s %v", status.Phase, err)
	}
	original, err := os.ReadFile(journalPath(f.engine.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(original, []byte(`"phase": "granted"`)) {
		t.Fatalf("rename did not publish grant: %s", original)
	}
	for range 2 {
		reopened, openErr := New(f.engine.cfg)
		if openErr != nil {
			t.Fatal(openErr)
		}
		before := calls
		_, err = reopened.Grant(context.Background(), f.request.ID, 1)
		if !errors.Is(err, fault) || calls == before {
			t.Fatalf("retry acknowledged unconfirmed journal: barriers=%d error=%v", calls-before, err)
		}
	}
	syncDirectory = syncReal
	status, err = f.engine.Grant(context.Background(), f.request.ID, 1)
	if err != nil || status.Phase != Granted || status.Request.Generation != 1 || status.RetryToken != 0 {
		t.Fatalf("settlement = %+v %v", status, err)
	}
	after, err := os.ReadFile(journalPath(f.engine.cfg.StateDir))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("grant retransmission changed journal: %v", err)
	}
	if f.manager.stops != 0 {
		t.Fatal("grant retransmission activated daemon")
	}
	f.roundTrip()
}
