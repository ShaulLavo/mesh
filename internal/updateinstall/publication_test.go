package updateinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if _, err := f.engine.Run(context.Background()); !errors.Is(err, fault) {
		t.Fatalf("helper consumed unconfirmed grant: %v", err)
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

func TestJournalFirstPublicationSyncsParents(t *testing.T) {
	for index := range 3 {
		t.Run(fmt.Sprint(index), func(t *testing.T) { testJournalParentBarrier(t, index) })
	}
}

func testJournalParentBarrier(t *testing.T, index int) {
	f := newFixture(t)
	state := f.engine.cfg.StateDir
	parent, err := filepath.EvalSymlinks(filepath.Dir(state))
	if err != nil {
		t.Fatal(err)
	}
	directories := []string{transactionDir(state), filepath.Join(parent, filepath.Base(state)), parent}
	syncReal := syncDirectory
	t.Cleanup(func() { syncDirectory = syncReal })
	fault := errors.New("injected first journal parent barrier failure")
	syncDirectory = func(path string) error {
		if path == directories[index] {
			return fault
		}
		return syncReal(path)
	}
	status, err := f.engine.Stage(context.Background(), f.request)
	if !errors.Is(err, fault) || status.Phase != Accepted {
		t.Fatalf("first acceptance = %s %v", status.Phase, err)
	}
	if _, err := f.engine.Stage(context.Background(), f.request); !errors.Is(err, fault) {
		t.Fatalf("acceptance replay = %v", err)
	}
	syncDirectory = syncReal
	status, err = f.engine.Stage(context.Background(), f.request)
	if err != nil || status.Phase != Staged || !sameRequest(status, f.request) {
		t.Fatalf("settled staging = %+v %v", status, err)
	}
	if f.manager.stops != 0 {
		t.Fatal("staging activated daemon")
	}
	f.roundTrip()
}

func TestJournalReplaysSettleWithoutNewAuthority(t *testing.T) {
	for _, action := range []string{"stage", "cancel", "retry"} {
		t.Run(action, func(t *testing.T) { testJournalReplayBarrier(t, action) })
	}
}

func journalReplayAction(t *testing.T, f *fixture, action string) (func() (Status, error), Phase) {
	t.Helper()
	if action == "stage" {
		return func() (Status, error) { return f.engine.Stage(context.Background(), f.request) }, Staged
	}
	status := f.stage()
	if action == "cancel" {
		return func() (Status, error) { return f.engine.Cancel(context.Background(), f.request.ID, 1) }, Cancelled
	}
	status.Phase, status.Error = Failed, "pre-activation failure"
	if err := f.engine.save(&status); err != nil {
		t.Fatal(err)
	}
	return func() (Status, error) { return f.engine.RetryWithToken(context.Background(), f.request.ID, 1, 7) }, Accepted
}

func testJournalReplayBarrier(t *testing.T, action string) {
	f := newFixture(t)
	invoke, phase := journalReplayAction(t, f, action)
	syncReal := syncDirectory
	t.Cleanup(func() { syncDirectory = syncReal })
	fault := errors.New("injected replay barrier failure")
	syncDirectory = func(path string) error {
		if path != transactionDir(f.engine.cfg.StateDir) {
			return syncReal(path)
		}
		data, err := os.ReadFile(journalPath(f.engine.cfg.StateDir))
		if err != nil {
			return fmt.Errorf("read replay journal at barrier: %w", err)
		}
		if bytes.Contains(data, []byte(fmt.Sprintf(`"phase": %q`, phase))) {
			return fault
		}
		return syncReal(path)
	}
	status, err := invoke()
	if !errors.Is(err, fault) || status.Phase != phase {
		t.Fatalf("publication = %s %v", status.Phase, err)
	}
	original, err := os.ReadFile(journalPath(f.engine.cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(); !errors.Is(err, fault) {
		t.Fatalf("unconfirmed replay = %v", err)
	}
	syncDirectory = syncReal
	status, err = invoke()
	if err != nil || status.Phase != phase || !sameRequest(status, f.request) {
		t.Fatalf("settled replay = %+v %v", status, err)
	}
	after, err := os.ReadFile(journalPath(f.engine.cfg.StateDir))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("replay changed journal bytes: %v", err)
	}
	if action == "retry" && status.RetryToken != 7 {
		t.Fatalf("retry token = %d", status.RetryToken)
	}
	if action == "cancel" {
		_, err = f.engine.Grant(context.Background(), f.request.ID, 1)
		if !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("cancelled grant = %v", err)
		}
	}
	if f.manager.stops != 0 {
		t.Fatal("replay activated daemon")
	}
	f.roundTrip()
}

func TestNestedJournalPublicationAncestry(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) { testNestedJournalPublication(t, alias) })
	}
}

func testNestedJournalPublication(t *testing.T, alias bool) {
	f := newFixture(t)
	root := t.TempDir()
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	base := root
	if alias {
		base = filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(root, base); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(base, "new", "parent", "state")
	f.engine.cfg.StateDir, f.manager.stateDir = state, state
	faultPath := filepath.Join(physical, "new")
	realSync := syncDirectory
	t.Cleanup(func() { syncDirectory = realSync })
	fault := errors.New("injected nested journal ancestor failure")
	calls := 0
	syncDirectory = func(path string) error {
		if path == faultPath {
			calls++
			return fault
		}
		return realSync(path)
	}
	status, err := f.engine.Stage(context.Background(), f.request)
	if !errors.Is(err, fault) || status.Phase != Accepted || calls == 0 {
		t.Fatalf("nested journal acknowledged without ancestor barrier: phase=%s calls=%d error=%v", status.Phase, calls, err)
	}
	for range 2 {
		reopened, openErr := New(f.engine.cfg)
		if openErr != nil {
			t.Fatal(openErr)
		}
		before := calls
		if _, err := reopened.Stage(context.Background(), f.request); !errors.Is(err, fault) || calls == before {
			t.Fatalf("nested journal replay lost ancestor barrier: calls=%d error=%v", calls-before, err)
		}
	}
	syncDirectory = realSync
	status, err = f.engine.Stage(context.Background(), f.request)
	if err != nil || status.Phase != Staged || !sameRequest(status, f.request) || f.manager.stops != 0 {
		t.Fatalf("nested staging settlement = %+v %v", status, err)
	}
	f.roundTrip()
}
