package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

const historyLabel = "serve /dev"

var historyBase = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// historyCatalog is a real catalog over SQLite whose clock stays at
// historyBase. Post-start prune tests seed rows without periodic retention.
type historyCatalog struct {
	root    string
	store   *storage.Store
	catalog *Catalog
}

func newHistoryCatalog(t *testing.T, wrap func(CatalogStore) CatalogStore) historyCatalog {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var catalogStore CatalogStore = store
	if wrap != nil {
		catalogStore = wrap(store)
	}
	root := t.TempDir()
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: root,
		Host:        catalogTestHost(historyBase),
		Store:       catalogStore,
		Probe:       probeFunc(func(context.Context, string) error { return nil }),
		BootID:      func() string { return "boot-a" },
		Now:         func() time.Time { return historyBase },
	})
	if err != nil {
		t.Fatal(err)
	}
	return historyCatalog{root: root, store: store, catalog: catalog}
}

func (h historyCatalog) exited(t *testing.T, id, label string, created time.Time, ran time.Duration) string {
	t.Helper()
	exitedAt, code := created.Add(ran), 1
	meta := worker.Meta{
		Reservation: &session.Reservation{ID: id, Instance: "0123456789abcdef0123456789abcdef", BootID: "boot-a", Purpose: session.Managed},
		ID:          id, PID: 4242, Command: []string{"sh", "-lc", "serve"}, Cwd: "/tmp", Label: label,
		State: worker.StateExited, CreatedAt: created, ExitedAt: &exitedAt, ExitCode: &code, BootID: "boot-a",
	}
	writeCatalogMeta(t, h.root, id, meta)
	return filepath.Join(h.root, id)
}

func (h historyCatalog) running(t *testing.T, id, label, boot string, created time.Time) {
	t.Helper()
	meta := worker.Meta{
		Reservation: &session.Reservation{ID: id, Instance: "0123456789abcdef0123456789abcdef", BootID: boot, Purpose: session.Managed},
		ID:          id, PID: 4242, Command: []string{"sh", "-lc", "serve"}, Cwd: "/tmp", Label: label,
		State: worker.StateRunning, CreatedAt: created, BootID: boot,
	}
	writeCatalogMeta(t, h.root, id, meta)
}

func (h historyCatalog) reconcile(t *testing.T) {
	t.Helper()
	if err := h.catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (h historyCatalog) prune(t *testing.T, current string) ([]string, error) {
	t.Helper()
	removed, err := h.catalog.PruneLabelled(context.Background(), historyLabel, storage.SessionID(current))
	ids := make([]string, len(removed))
	for index, id := range removed {
		ids[index] = string(id)
	}
	slices.Sort(ids)
	return ids, err
}

func (h historyCatalog) assertKept(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := os.Stat(paths.Meta(filepath.Join(h.root, id))); err != nil {
			t.Fatalf("session %s directory was removed: %v", id, err)
		}
		if _, err := h.store.GetSession(context.Background(), h.catalog.host.ID, storage.SessionID(id)); err != nil {
			t.Fatalf("session %s row was removed: %v", id, err)
		}
	}
}

func (h historyCatalog) assertRemoved(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(h.root, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("session %s directory survived: %v", id, err)
		}
		if _, err := h.store.GetSession(context.Background(), h.catalog.host.ID, storage.SessionID(id)); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("session %s row survived: %v", id, err)
		}
	}
}

func (h historyCatalog) assertNoClaimLeft(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := os.Lstat(filepath.Join(h.root, id, "recovery.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("session %s was left with a recovery lock: %v", id, err)
		}
	}
}

func historyShorts(t *testing.T, h historyCatalog, count int) []string {
	t.Helper()
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("H%03d", index+1)
		h.exited(t, ids[index], historyLabel, historyBase.Add(-time.Duration(index+1)*time.Minute), 2*time.Second)
	}
	return ids
}

// A route keeps its ten newest attempts and its newest long run. The
// current attempt, other labels, live and uncertain sessions, owner pins
// and every kind of recovery history are not candidates at all.
func TestPruneLabelledKeepsBoundedHistoryAndProtectedRecords(t *testing.T) {
	h := newHistoryCatalog(t, nil)
	h.exited(t, "C0RR", historyLabel, historyBase.Add(-time.Second), time.Second)
	shorts := historyShorts(t, h, 12)
	h.exited(t, "G001", historyLabel, historyBase.Add(-2*time.Hour), 30*time.Minute)
	h.exited(t, "G002", historyLabel, historyBase.Add(-3*time.Hour), 10*time.Minute)
	h.exited(t, "B001", "serve /api", historyBase.Add(-4*time.Hour), time.Second)
	h.running(t, "V001", historyLabel, "boot-a", historyBase.Add(-5*time.Hour))
	h.running(t, "N001", historyLabel, "previous-boot", historyBase.Add(-5*time.Hour))
	protected := map[string]func(dir string){
		"P001": func(dir string) { writeHistoryFile(t, filepath.Join(dir, ".owner")) },
		"R001": func(dir string) { writeHistoryFile(t, filepath.Join(dir, "recovery.json")) },
		"R002": func(dir string) { writeHistoryFile(t, filepath.Join(dir, "recovery-intent.json")) },
		"R003": func(dir string) { writeHistoryFile(t, filepath.Join(dir, "recovery-command.json")) },
		"R004": func(dir string) { writeHistoryFile(t, filepath.Join(dir, "recovery.lock")) },
		"R005": func(dir string) { recoveredFrom(t, dir) },
	}
	for id, protect := range protected {
		protect(h.exited(t, id, historyLabel, historyBase.Add(-6*time.Hour), time.Second))
	}
	h.seed(t)
	removed, err := h.prune(t, "C0RR")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"G002", "H011", "H012", "N001"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	h.assertRemoved(t, removed...)
	h.assertKept(t, append(shorts[:10], "C0RR", "G001", "B001", "V001", "P001", "R001", "R002", "R003", "R004", "R005")...)
	h.assertNoClaimLeft(t, append(shorts[:10], "C0RR", "G001")...)
	removed, err = h.prune(t, "C0RR")
	if err != nil || len(removed) != 0 {
		t.Fatalf("a second prune removed %v, %v; want nothing more", removed, err)
	}
}

func writeHistoryFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recoveredFrom(t *testing.T, dir string) {
	t.Helper()
	meta, err := worker.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	meta.RecoveredFrom = "H001"
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
}

// claimingStore lets recovery claim a later candidate while the prune is
// already retiring an earlier one, after both were selected.
type claimingStore struct {
	CatalogStore
	once          sync.Once
	claim         func()
	fail          storage.SessionID
	beforeFailure func()
}

func (s *claimingStore) RetireSessions(ctx context.Context, host storage.HostID, ids []storage.SessionID) (int64, error) {
	if slices.Contains(ids, s.fail) {
		if s.beforeFailure != nil {
			s.beforeFailure()
		}
		return 0, errors.New("database is locked")
	}
	if s.claim != nil {
		s.once.Do(s.claim)
	}
	removed, err := s.CatalogStore.RetireSessions(ctx, host, ids)
	if err != nil {
		return removed, fmt.Errorf("retire: %w", err)
	}
	return removed, nil
}

// lockLikeRecovery takes a session's recovery lock the way recovery does and
// holds it until the test ends.
func lockLikeRecovery(t *testing.T, dir string) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile("recovery.lock", os.O_CREATE|os.O_RDWR, 0o600)
	_ = root.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	})
}

func TestPruneLabelledSkipsACandidateRecoveryClaimsAfterSelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		claim func(t *testing.T, dir string)
	}{
		{name: "recovery lock", claim: lockLikeRecovery},
		{name: "saved recovery", claim: func(t *testing.T, dir string) { writeHistoryFile(t, filepath.Join(dir, "recovery-intent.json")) }},
		{name: "recovered from", claim: recoveredFrom},
		{name: "owner pin", claim: func(t *testing.T, dir string) { writeHistoryFile(t, filepath.Join(dir, ".owner")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertPruningRespectsClaim(t, test.name, test.claim)
		})
	}
}

// A failed retirement leaves the session exactly as it was: row, directory,
// and no recovery lock that would protect it forever.
func assertPruningRespectsClaim(t *testing.T, name string, claim func(*testing.T, string)) {
	t.Helper()
	var store *claimingStore
	h := newHistoryCatalog(t, func(inner CatalogStore) CatalogStore {
		store = &claimingStore{CatalogStore: inner}
		return store
	})
	historyShorts(t, h, 13)
	h.seed(t)
	store.claim = func() { claim(t, filepath.Join(h.root, "H012")) }
	removed, err := h.prune(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"H011", "H013"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v with H012 kept for recovery", removed, want)
	}
	h.assertKept(t, "H012")
	if name == "recovery lock" {
		if _, err := os.Lstat(filepath.Join(h.root, "H012", "recovery.lock")); err != nil {
			t.Fatalf("prune removed recovery's own lock: %v", err)
		}
		return
	}
	h.assertNoClaimLeft(t, "H012")
}

func TestPruneLabelledFailedRetirementLeavesNoMarker(t *testing.T) {
	h := newHistoryCatalog(t, func(inner CatalogStore) CatalogStore {
		return &claimingStore{CatalogStore: inner, fail: "H011"}
	})
	historyShorts(t, h, 11)
	h.seed(t)
	removed, err := h.prune(t, "")
	if err == nil || len(removed) != 0 {
		t.Fatalf("prune = %v, %v; want the retirement failure and nothing removed", removed, err)
	}
	h.assertKept(t, "H011")
	h.assertNoClaimLeft(t, "H011")
}

// App Forget stays the explicit, destructive operation: it removes ended
// records that automatic pruning protects, and still refuses owner pins and
// live workers.
func TestAppForgetRemainsExplicitAndPinRefusing(t *testing.T) {
	h := newHistoryCatalog(t, nil)
	writeHistoryFile(t, filepath.Join(h.exited(t, "R001", "app x", historyBase.Add(-time.Hour), time.Second), "recovery.json"))
	writeHistoryFile(t, filepath.Join(h.exited(t, "P001", "app x", historyBase.Add(-time.Hour), time.Second), ".owner"))
	h.running(t, "V001", "app x", "boot-a", historyBase.Add(-time.Hour))
	h.reconcile(t)
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: h.catalog, Connector: failingLifecycleConnector(),
		Host: catalogTestHost(historyBase), SessionsDir: h.root,
	})
	lifecycle.forgetLabelled(context.Background(), "app x", "")
	h.assertRemoved(t, "R001")
	h.assertKept(t, "P001", "V001")
}

// Retention ages an ended session by when it exited, read from the metadata
// that produced the scan's answer, including the reread after a worker turns
// out to be gone. Each scan starts its exit times over.
func TestCatalogAgesFinishedSessionsByExitTime(t *testing.T) {
	root := t.TempDir()
	now := historyBase
	exited := func(id string, created, exitedAt time.Time) worker.Meta {
		meta := catalogTestMeta(id, worker.StateExited, "boot-a")
		code := 0
		meta.CreatedAt, meta.ExitedAt, meta.ExitCode = created, &exitedAt, &code
		return meta
	}
	writeCatalogMeta(t, root, "0DDX", exited("0DDX", now.Add(-30*24*time.Hour), now.Add(-time.Hour)))
	writeCatalogMeta(t, root, "0DDY", exited("0DDY", now.Add(-30*24*time.Hour), now.Add(-8*24*time.Hour)))
	running := catalogTestMeta("REXT", worker.StateRunning, "boot-a")
	running.CreatedAt = now.Add(-30 * 24 * time.Hour)
	writeCatalogMeta(t, root, "REXT", running)
	rereadExit := now.Add(-2 * time.Hour)
	store := &catalogStoreStub{}
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: root, Host: catalogTestHost(now), Store: store, BootID: func() string { return "boot-a" },
		Now: func() time.Time { return now },
		Probe: probeFunc(func(_ context.Context, socket string) error {
			dir := filepath.Dir(socket)
			if filepath.Base(dir) == "REXT" {
				_ = worker.WriteMeta(dir, exited("REXT", running.CreatedAt, rereadExit))
				return syscall.ENOENT
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.retired, []storage.SessionID{"0DDY"}) {
		t.Fatalf("retired %v, want only the session that exited more than a week ago", store.retired)
	}
	if got := catalog.scannedExitedAt["REXT"]; !got.Equal(rereadExit) {
		t.Fatalf("exit time of a reread session = %v, want %v from the reread", got, rereadExit)
	}
	if err := os.RemoveAll(filepath.Join(root, "REXT")); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, stale := catalog.scannedExitedAt["REXT"]; stale {
		t.Fatal("a later scan kept an exit time for a session it no longer saw")
	}
}

func TestCatalogRejectsAnExitBeforeItsCreation(t *testing.T) {
	meta := catalogTestMeta("7K3D", worker.StateExited, "boot-a")
	exitedAt, code := meta.CreatedAt.Add(-time.Second), 0
	meta.ExitedAt, meta.ExitCode = &exitedAt, &code
	if _, _, err := sessionFromMeta("host-id", "7K3D", meta); err == nil {
		t.Fatal("an exit before its creation was accepted")
	}
}

// The count cap retires the sessions that ended longest ago, whatever their
// creation order, and falls back to creation for one with no exit time.
func TestCatalogCountCapRanksByExitTime(t *testing.T) {
	catalog := &Catalog{sessionsDir: t.TempDir(), now: func() time.Time { return historyBase }, scannedExitedAt: map[storage.SessionID]time.Time{}}
	count := maxRetainedTerminalSessions + 2
	observed := make([]storage.Session, count)
	for index := range observed {
		id := storage.SessionID(fmt.Sprintf("%04d", index))
		// Created oldest first, but exited newest first.
		observed[index] = storage.Session{ID: id, State: storage.StateExited, CreatedAt: historyBase.Add(-time.Duration(count-index) * time.Hour)}
		catalog.scannedExitedAt[id] = historyBase.Add(-time.Duration(index+1) * time.Minute)
	}
	_, retired := catalog.retireFinishedSessions(observed)
	want := []storage.SessionID{storage.SessionID(fmt.Sprintf("%04d", count-2)), storage.SessionID(fmt.Sprintf("%04d", count-1))}
	if !slices.Equal(retired, want) {
		t.Fatalf("retired %v, want the two that exited first %v", retired, want)
	}
	delete(catalog.scannedExitedAt, observed[0].ID)
	_, retired = catalog.retireFinishedSessions(observed)
	if !slices.Contains(retired, observed[0].ID) {
		t.Fatalf("retired %v; a session with no exit time did not fall back to its creation time", retired)
	}
}

// These names deliberately have no service definitions and never start again.
func TestPeriodicLabelledHistoryBoundsDisabledAndDeletedNames(t *testing.T) {
	for _, label := range []string{"serve /disabled", "service deleted"} {
		t.Run(label, func(t *testing.T) { assertPeriodicLabelledHistory(t, label) })
	}
}

func assertPeriodicLabelledHistory(t *testing.T, label string) {
	t.Helper()
	h := newHistoryCatalog(t, nil)
	for index := range 12 {
		h.running(t, fmt.Sprintf("B%03d", index), label, "previous-boot", historyBase.Add(-time.Duration(index+1)*time.Minute))
	}
	h.exited(t, "G001", label, historyBase.Add(-2*time.Hour), 10*time.Minute)
	h.exited(t, "G002", label, historyBase.Add(-3*time.Hour), 10*time.Minute)
	h.reconcile(t)
	h.assertRemoved(t, "B010", "B011", "G002")
	h.assertKept(t, "B000", "B009", "G001")
	h.reconcile(t)
	h.assertKept(t, "B009", "G001")
}

func TestPruneLabelledBoundsPositivelyOldBootHistory(t *testing.T) {
	h := newHistoryCatalog(t, nil)
	for index := range 12 {
		h.running(t, fmt.Sprintf("B%03d", index), historyLabel, "previous-boot", historyBase.Add(-time.Duration(index+1)*time.Minute))
	}
	h.exited(t, "G001", historyLabel, historyBase.Add(-time.Hour), 10*time.Minute)
	h.seed(t)
	removed, err := h.prune(t, "")
	if err != nil || !slices.Equal(removed, []string{"B010", "B011"}) {
		t.Fatalf("prune = %v, %v", removed, err)
	}
	h.assertKept(t, "B009", "G001")
}

func (h historyCatalog) seed(t *testing.T) {
	t.Helper()
	observed, err := h.catalog.scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]storage.ObservedSession, len(observed))
	for i, session := range observed {
		rows[i].Session = session
	}
	if err := h.store.ReconcileHost(t.Context(), h.catalog.host, rows); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicLabelledHistoryRequiresPositiveOwnershipEvidence(t *testing.T) {
	for _, test := range []struct{ name, boot, protection string }{
		{"current boot", "boot-a", ""}, {"unknown recorded boot", "", ""},
		{"unknown host boot", "previous-boot", "host"}, {"torn metadata", "previous-boot", "torn"},
		{"owner pin", "previous-boot", ".owner"}, {"recovery", "previous-boot", "recovery.json"},
		{"recovery intent", "previous-boot", "recovery-intent.json"}, {"recovery command", "previous-boot", "recovery-command.json"},
		{"recovery lock", "previous-boot", "recovery.lock"}, {"recovered from", "previous-boot", "source"},
		{"unlabelled", "previous-boot", "label"},
	} {
		t.Run(test.name, func(t *testing.T) { assertPeriodicHistoryProtection(t, test.boot, test.protection) })
	}
}

func assertPeriodicHistoryProtection(t *testing.T, boot, protection string) {
	t.Helper()
	h := newHistoryCatalog(t, nil)
	h.catalog.probe = probeFunc(func(context.Context, string) error { return syscall.ENOENT })
	historyShorts(t, h, 12)
	label := historyLabel
	if protection == "label" {
		label = ""
	}
	h.running(t, "P001", label, boot, historyBase.Add(-30*24*time.Hour))
	dir := filepath.Join(h.root, "P001")
	switch protection {
	case "host":
		h.catalog.bootID = func() string { return "" }
	case "torn":
		if err := os.WriteFile(paths.Meta(dir), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "source":
		recoveredFrom(t, dir)
	case "", "label":
	default:
		writeHistoryFile(t, filepath.Join(dir, protection))
	}
	h.reconcile(t)
	h.assertKept(t, "P001")
	_, err := h.prune(t, "")
	if err != nil {
		t.Fatal(err)
	}
	h.assertKept(t, "P001")
}

func TestPeriodicOldBootHistoryUsesCountWithoutInventingExitTime(t *testing.T) {
	h := newHistoryCatalog(t, nil)
	for index := range maxRetainedTerminalSessions + 2 {
		h.running(t, fmt.Sprintf("B%03d", index), fmt.Sprintf("service removed-%d", index), "previous-boot", historyBase.Add(-time.Duration(index+1)*24*time.Hour))
	}
	h.reconcile(t)
	h.assertRemoved(t, "B500", "B501")
	h.assertKept(t, "B000", "B499")
	h.catalog.now = func() time.Time { return historyBase.Add(90 * 24 * time.Hour) }
	h.reconcile(t)
	h.assertKept(t, "B499")
}

func TestPeriodicLabelledHistoryStillAgesKnownExits(t *testing.T) {
	h := newHistoryCatalog(t, nil)
	h.exited(t, "G001", historyLabel, historyBase.Add(-30*24*time.Hour), 10*time.Minute)
	h.running(t, "B001", historyLabel, "previous-boot", historyBase.Add(-40*24*time.Hour))
	h.reconcile(t)
	h.assertRemoved(t, "G001")
	h.assertKept(t, "B001")
}
