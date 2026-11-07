package identity

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type grantAckFault struct {
	*os.File
	events                []string
	truncates             int
	failTruncate          int
	syncError, writeError error
	writeLimit            int
}

func (f *grantAckFault) Truncate(size int64) error {
	f.events = append(f.events, "truncate")
	f.truncates++
	if f.truncates == f.failTruncate {
		return os.ErrPermission
	}
	return grantFixtureError(f.File.Truncate(size))
}

func (f *grantAckFault) Sync() error {
	f.events = append(f.events, "invalidate-sync")
	if f.syncError != nil {
		return f.syncError
	}
	return grantFixtureError(f.File.Sync())
}

func (f *grantAckFault) WriteAt(contents []byte, offset int64) (int, error) {
	f.events = append(f.events, "ack-write")
	n := len(contents)
	if f.writeLimit >= 0 {
		n = f.writeLimit
	}
	written, err := f.File.WriteAt(contents[:n], offset)
	if err != nil {
		return written, grantFixtureError(err)
	}
	return written, f.writeError
}

func grantAckFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "receipt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestGrantReceiptReadBoundaries(t *testing.T) {
	digest := sha256.Sum256([]byte("exact policy\n"))
	for _, receipt := range [][]byte{nil, digest[:5], bytes.Repeat([]byte{1}, 32), append(digest[:], 0), bytes.Repeat([]byte{2}, 4096), digest[:]} {
		file := grantAckFile(t)
		if _, err := file.WriteAt(receipt, 0); err != nil {
			t.Fatal(err)
		}
		acknowledged, err := grantsAcknowledged(file, digest)
		if err != nil || acknowledged != bytes.Equal(receipt, digest[:]) {
			t.Fatalf("length=%d: acknowledged=%v, error=%v", len(receipt), acknowledged, err)
		}
	}
	file := grantAckFile(t)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := grantsAcknowledged(file, digest); acknowledged || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: acknowledged=%v, error=%v", acknowledged, err)
	}
}

func TestGrantAcknowledgementFailureOrdering(t *testing.T) {
	cause := errors.New("acknowledgement I/O failed")
	for _, name := range []string{"invalidate-truncate", "invalidate-sync", "ack-truncate", "partial-error", "short-write", "full-error", "success"} {
		t.Run(name, func(t *testing.T) { checkGrantAckFailure(t, name, cause) })
	}
}

func checkGrantAckFailure(t *testing.T, name string, cause error) {
	t.Helper()
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	old := grantBytes(t, state, "authorized_keys")
	next := append(bytes.Clone(old), []byte("# next policy\n")...)
	digest := sha256.Sum256(next)
	lock, err := grantFixtureRoot(t, state).OpenFile("device-grants.lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	fault := &grantAckFault{File: lock, writeLimit: -1}
	wantError := configureGrantAckFault(fault, name, cause)
	publications := 0
	err = persistGrants(fault, digest, func() error {
		publications++
		fault.events = append(fault.events, "publish")
		return publishGrants(state, filepath.Join(state, "authorized_keys"), next)
	})
	if !errors.Is(err, wantError) {
		t.Fatalf("error=%v, want %v", err, wantError)
	}
	wantPublications := 1
	if name == "invalidate-truncate" || name == "invalidate-sync" {
		wantPublications = 0
	}
	if publications != wantPublications {
		t.Fatalf("publications=%d", publications)
	}
	if wantPublications == 0 && !bytes.Equal(old, grantBytes(t, state, "authorized_keys")) {
		t.Fatal("invalidation failure changed policy")
	}
	acknowledged, err := grantsAcknowledged(lock, digest)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged != (name == "full-error" || name == "success") {
		t.Fatalf("acknowledged=%v", acknowledged)
	}
	if name == "success" && !reflect.DeepEqual(fault.events, []string{"truncate", "invalidate-sync", "publish", "truncate", "ack-write"}) {
		t.Fatalf("events=%v", fault.events)
	}
	if wantPublications == 0 {
		return
	}
	probe := new(grantPublicationProbe)
	changed, err := changeDeviceWithPublisher(t.Context(), state, id, true, probe.publish)
	if changed || err != nil {
		t.Fatalf("independent call: changed=%v, error=%v", changed, err)
	}
	wantBarriers := 1
	if acknowledged {
		wantBarriers = 0
	}
	if probe.directories != wantBarriers {
		t.Fatalf("independent barriers=%d, want %d", probe.directories, wantBarriers)
	}
}

func configureGrantAckFault(fault *grantAckFault, name string, cause error) error {
	switch name {
	case "invalidate-truncate":
		fault.failTruncate = 1
		return os.ErrPermission
	case "invalidate-sync":
		fault.syncError = cause
		return cause
	case "ack-truncate":
		fault.failTruncate = 2
		return os.ErrPermission
	case "partial-error":
		fault.writeLimit = 7
		fault.writeError = cause
		return cause
	case "short-write":
		fault.writeLimit = 7
		return io.ErrShortWrite
	case "full-error":
		fault.writeError = cause
		return cause
	default:
		return nil
	}
}

func TestGrantPersistenceOrdersRealBarriers(t *testing.T) {
	state := t.TempDir()
	lock := &grantAckFault{File: grantAckFile(t), writeLimit: -1}
	policy := []byte("# policy\n")
	digest := sha256.Sum256(policy)
	err := persistGrants(lock, digest, func() error {
		return publishGrantsWithSync(state, filepath.Join(state, "authorized_keys"), policy, lock.syncPublished)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"truncate", "invalidate-sync", "policy-sync", "directory-sync", "truncate", "ack-write"}
	if !reflect.DeepEqual(lock.events, want) {
		t.Fatalf("events=%v, want %v", lock.events, want)
	}
}

func (f *grantAckFault) syncPublished(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return grantFixtureError(err)
	}
	event := "policy-sync"
	if info.IsDir() {
		event = "directory-sync"
	}
	f.events = append(f.events, event)
	return grantFixtureError(file.Sync())
}

type grantReadFailure struct {
	digest [sha256.Size]byte
	cause  error
}

func (r grantReadFailure) ReadAt(buffer []byte, _ int64) (int, error) {
	return copy(buffer, r.digest[:]), r.cause
}

func TestGrantReceiptReadErrorOverridesMatchingBuffer(t *testing.T) {
	digest := sha256.Sum256([]byte("policy"))
	cause := errors.New("receipt read failed")
	acknowledged, err := grantsAcknowledged(grantReadFailure{digest, cause}, digest)
	if acknowledged || !errors.Is(err, cause) {
		t.Fatalf("acknowledged=%v, error=%v", acknowledged, err)
	}
}

type grantCrashModel struct {
	receipt, stableReceipt []byte
	policy, stablePolicy   []byte
	stop                   string
}

func (m *grantCrashModel) Truncate(_ int64) error {
	if m.stop == "before-invalidation" {
		return os.ErrPermission
	}
	m.receipt = nil
	return nil
}

func (m *grantCrashModel) Sync() error {
	if m.stop == "invalidation-sync" {
		return os.ErrPermission
	}
	m.stableReceipt = bytes.Clone(m.receipt)
	return nil
}

func (m *grantCrashModel) WriteAt(contents []byte, _ int64) (int, error) {
	if m.stop == "partial-receipt" {
		m.receipt = bytes.Clone(contents[:7])
		return 7, os.ErrPermission
	}
	m.receipt = bytes.Clone(contents)
	return len(contents), nil
}

func (m *grantCrashModel) publish(next []byte) error {
	if m.stop == "invalidated" {
		return os.ErrPermission
	}
	m.policy = bytes.Clone(next)
	if m.stop == "renamed" {
		return os.ErrPermission
	}
	m.stablePolicy = bytes.Clone(next)
	if m.stop == "directory-synced" {
		return os.ErrPermission
	}
	return nil
}

func TestGrantCrashPersistenceModel(t *testing.T) {
	old, next := []byte("policy A"), []byte("policy B")
	oldDigest, nextDigest := sha256.Sum256(old), sha256.Sum256(next)
	for _, stop := range []string{"before-invalidation", "invalidation-sync", "invalidated", "renamed", "directory-synced", "partial-receipt", "success"} {
		model := &grantCrashModel{receipt: bytes.Clone(oldDigest[:]), stableReceipt: bytes.Clone(oldDigest[:]), policy: old, stablePolicy: old, stop: stop}
		err := persistGrants(model, nextDigest, func() error { return model.publish(next) })
		if (err == nil) != (stop == "success") {
			t.Fatalf("%s: error=%v", stop, err)
		}
		checkGrantCrashReceipts(t, model)
	}
}

func checkGrantCrashReceipts(t *testing.T, model *grantCrashModel) {
	t.Helper()
	// Before directory sync, either the old or renamed inode may survive. The
	// unsynced receipt may survive, disappear, or retain its last synced contents.
	for _, policy := range [][]byte{model.policy, model.stablePolicy} {
		for _, receipt := range [][]byte{nil, model.receipt, model.stableReceipt} {
			checkGrantCrashReceipt(t, model, policy, receipt)
		}
	}
}

func checkGrantCrashReceipt(t *testing.T, model *grantCrashModel, policy, receipt []byte) {
	t.Helper()
	digest := sha256.Sum256(policy)
	if len(receipt) != sha256.Size || !bytes.Equal(receipt, digest[:]) {
		return
	}
	if !bytes.Equal(policy, model.stablePolicy) {
		t.Fatalf("%s: receipt certified policy without directory durability", model.stop)
	}
}
