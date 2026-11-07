package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type grantPublicationProbe struct {
	files, directories int
	directoryError     error
}

func (p *grantPublicationProbe) publish(state, path string, next []byte) error {
	return publishGrantsWithSync(state, path, next, p.sync)
}

func (p *grantPublicationProbe) sync(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return grantFixtureError(err)
	}
	if info.IsDir() {
		p.directories++
		if p.directoryError != nil {
			return p.directoryError
		}
	} else {
		p.files++
	}
	return grantFixtureError(file.Sync())
}

func grantActor(t *testing.T) string {
	t.Helper()
	actor, _, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return actor.ID
}

func grantBytes(t *testing.T, state, name string) []byte {
	t.Helper()
	contents, err := grantFixtureRoot(t, state).ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestGrantAcknowledgedDuplicatesPublishOnce(t *testing.T) {
	state, id := t.TempDir(), grantActor(t)
	probe := new(grantPublicationProbe)
	var first os.FileInfo
	for turn := range 4 {
		changed, err := changeDeviceWithPublisher(t.Context(), state, id, true, probe.publish)
		if err != nil || changed != (turn == 0) {
			t.Fatalf("turn %d: changed=%v, error=%v", turn, changed, err)
		}
		current, err := os.Stat(filepath.Join(state, "authorized_keys"))
		if err != nil {
			t.Fatal(err)
		}
		if turn == 0 {
			first = current
			pinGrantPolicy(t, state)
		}
		if !os.SameFile(first, current) {
			t.Errorf("turn %d republished the acknowledged policy", turn)
		}
	}
	if probe.files != 1 || probe.directories != 1 {
		t.Fatalf("publication barriers: files=%d, directories=%d, want one each", probe.files, probe.directories)
	}
}

func TestGrantFailedDirectoryNeverAcknowledgesVisiblePolicy(t *testing.T) {
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	old := grantBytes(t, state, "authorized_keys")
	digest := sha256.Sum256(old)
	if err := os.WriteFile(filepath.Join(state, "device-grants.lock"), digest[:], 0600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("directory barrier failed")
	probe := &grantPublicationProbe{directoryError: cause}
	other := grantActor(t)
	for turn := range 2 {
		changed, err := changeDeviceWithPublisher(t.Context(), state, other, true, probe.publish)
		if changed || !errors.Is(err, cause) {
			t.Fatalf("turn %d: changed=%v, error=%v", turn, changed, err)
		}
		if !GrantedIdentity(state, other) {
			t.Fatal("failure seam did not execute real rename")
		}
		if receipt := grantBytes(t, state, "device-grants.lock"); len(receipt) != 0 {
			t.Errorf("failed publication retained acknowledgement: %x", receipt)
		}
	}
	if probe.files != 2 || probe.directories != 2 {
		t.Fatal("equal independent call skipped failed directory barrier")
	}
	if changed, err := ApproveDeviceChanged(t.Context(), state, other); changed || err != nil {
		t.Fatalf("settlement changed=%v, error=%v", changed, err)
	}
}

func TestGrantFailedABACannotReuseOldAcknowledgement(t *testing.T) {
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	original := grantBytes(t, state, "authorized_keys")
	digest := sha256.Sum256(original)
	if err := os.WriteFile(filepath.Join(state, "device-grants.lock"), digest[:], 0600); err != nil {
		t.Fatal(err)
	}
	bound, ok := BindIdentity(state, id)
	if !ok {
		t.Fatal("original grant missing")
	}
	cause := errors.New("directory barrier failed")
	probe := &grantPublicationProbe{directoryError: cause}
	other := grantActor(t)
	for _, operation := range []struct {
		id      string
		approve bool
	}{{other, true}, {other, false}, {id, true}} {
		changed, err := changeDeviceWithPublisher(t.Context(), state, operation.id, operation.approve, probe.publish)
		if changed || !errors.Is(err, cause) {
			t.Fatalf("changed=%v, error=%v", changed, err)
		}
	}
	if !bytes.Equal(original, grantBytes(t, state, "authorized_keys")) {
		t.Fatal("A-B-A did not restore exact policy bytes")
	}
	if receipt := grantBytes(t, state, "device-grants.lock"); len(receipt) != 0 {
		t.Fatal("failed A-B-A retained acknowledgement")
	}
	if probe.directories != 3 || !bound() {
		t.Fatal("A-B-A skipped publication or retired surviving grant")
	}
	if changed, err := ApproveDeviceChanged(t.Context(), state, id); changed || err != nil {
		t.Fatalf("settlement changed=%v, error=%v", changed, err)
	}
}

func pinGrantPolicy(t *testing.T, state string) {
	t.Helper()
	file, err := grantFixtureRoot(t, state).Open("authorized_keys")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
}

func TestGrantReceiptMissRepublishesUnchangedPolicy(t *testing.T) {
	for _, kind := range []string{"missing", "short", "long", "nonmatch", "oversized"} {
		t.Run(kind, func(t *testing.T) { checkGrantReceiptMiss(t, kind) })
	}
}

func checkGrantReceiptMiss(t *testing.T, kind string) {
	t.Helper()
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	original := grantBytes(t, state, "authorized_keys")
	digest := sha256.Sum256(original)
	receipts := map[string][]byte{"missing": nil, "short": digest[:7], "long": append(digest[:], 0), "nonmatch": bytes.Repeat([]byte{0}, 32), "oversized": bytes.Repeat([]byte{0}, 1<<20)}
	if err := os.WriteFile(filepath.Join(state, "device-grants.lock"), receipts[kind], 0600); err != nil {
		t.Fatal(err)
	}
	probe := new(grantPublicationProbe)
	changed, err := changeDeviceWithPublisher(t.Context(), state, id, true, probe.publish)
	if changed || err != nil {
		t.Fatalf("changed=%v, error=%v", changed, err)
	}
	if probe.files != 1 || probe.directories != 1 {
		t.Fatal("receipt miss did not republish")
	}
	if !bytes.Equal(original, grantBytes(t, state, "authorized_keys")) {
		t.Fatal("durability-only publication changed grant incarnation")
	}
	current, err := os.Stat(filepath.Join(state, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	pinGrantPolicy(t, state)
	changed, err = changeDeviceWithPublisher(t.Context(), state, id, true, probe.publish)
	after, statErr := os.Stat(filepath.Join(state, "authorized_keys"))
	if changed || err != nil || statErr != nil || !os.SameFile(current, after) || probe.files != 1 {
		t.Fatalf("acknowledged no-op: changed=%v, error=%v, stat=%v, files=%d", changed, err, statErr, probe.files)
	}
}

func TestGrantMissingPolicyCannotUseEmptyReceipt(t *testing.T) {
	state, id := t.TempDir(), grantActor(t)
	digest := sha256.Sum256(nil)
	if err := os.WriteFile(filepath.Join(state, "device-grants.lock"), digest[:], 0600); err != nil {
		t.Fatal(err)
	}
	probe := new(grantPublicationProbe)
	changed, err := changeDeviceWithPublisher(t.Context(), state, id, false, probe.publish)
	if changed || err != nil {
		t.Fatalf("changed=%v, error=%v", changed, err)
	}
	if probe.directories != 1 || len(grantBytes(t, state, "authorized_keys")) != 0 {
		t.Fatal("missing policy bypassed publication")
	}
}

func TestGrantMatchingReceiptStillValidatesPolicy(t *testing.T) {
	for _, kind := range []string{"malformed", "oversized", "unsafe-policy", "unreadable-policy", "unsafe-lock", "invalid-identity"} {
		t.Run(kind, func(t *testing.T) { checkGrantInvalidReceiptPolicy(t, kind) })
	}
}

func checkGrantInvalidReceiptPolicy(t *testing.T, kind string) {
	t.Helper()
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	policy := grantBytes(t, state, "authorized_keys")
	switch kind {
	case "malformed":
		policy = append(policy, []byte("broken key\n")...)
	case "oversized":
		policy = append(policy, bytes.Repeat([]byte("#"), AuthorizedKeysMaximum)...)
	case "invalid-identity":
		id = "invalid"
	}
	path := filepath.Join(state, "authorized_keys")
	if err := os.WriteFile(path, policy, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(policy)
	lock := filepath.Join(state, "device-grants.lock")
	if err := os.WriteFile(lock, digest[:], 0600); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "unsafe-policy":
		if err := grantFixtureRoot(t, state).Chmod("authorized_keys", 0620); err != nil {
			t.Fatal(err)
		}
	case "unreadable-policy":
		if err := grantFixtureRoot(t, state).Chmod("authorized_keys", 0200); err != nil {
			t.Fatal(err)
		}
	case "unsafe-lock":
		if err := grantFixtureRoot(t, state).Chmod("device-grants.lock", 0620); err != nil {
			t.Fatal(err)
		}
	}
	probe := new(grantPublicationProbe)
	if changed, err := changeDeviceWithPublisher(t.Context(), state, id, true, probe.publish); changed || err == nil {
		t.Fatalf("changed=%v, error=%v", changed, err)
	}
	if probe.files != 0 {
		t.Fatal("rejected policy reached publication")
	}
}

func TestGrantMatchingReceiptStillTakesWriterLock(t *testing.T) {
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	lock, err := openGrantWriterContext(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	probe := new(grantPublicationProbe)
	changed, err := changeDeviceWithPublisher(t.Context(), state, id, true, probe.publish)
	if changed || !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("changed=%v, error=%v", changed, err)
	}
	if probe.files != 0 || !GrantedIdentity(state, id) {
		t.Fatal("contender published or snapshot reader waited for writer lock")
	}
}

func TestGrantPublicationFailuresPreserveUnacknowledgedState(t *testing.T) {
	for _, phase := range []string{"stage", "write", "file-sync", "close", "rename", "directory-open", "directory-sync"} {
		t.Run(phase, func(t *testing.T) { checkGrantPublicationFailure(t, phase) })
	}
}

func checkGrantPublicationFailure(t *testing.T, phase string) {
	t.Helper()
	state, id := t.TempDir(), grantActor(t)
	if err := ApproveDevice(t.Context(), state, id); err != nil {
		t.Fatal(err)
	}
	old := grantBytes(t, state, "authorized_keys")
	cause := errors.New("publication barrier failed")
	publisher := func(directory, path string, next []byte) error {
		return failGrantPublication(directory, path, next, phase, cause)
	}
	root := grantFixtureRoot(t, state)
	t.Cleanup(func() { _ = root.Chmod(".", 0700) })
	changed, err := changeDeviceWithPublisher(t.Context(), state, grantActor(t), true, publisher)
	if phase == "directory-open" {
		_ = root.Chmod(".", 0700)
	}
	if changed || err == nil {
		t.Fatalf("changed=%v, error=%v", changed, err)
	}
	wantError := cause
	switch phase {
	case "stage", "rename":
		wantError = os.ErrNotExist
	case "write":
		wantError = syscall.EBADF
	case "close":
		wantError = os.ErrClosed
	case "directory-open":
		wantError = os.ErrPermission
	}
	if !errors.Is(err, wantError) {
		t.Fatalf("lost cause: %v, want %v", err, wantError)
	}
	if len(grantBytes(t, state, "device-grants.lock")) != 0 {
		t.Fatal("failed publication acknowledged")
	}
	afterRename := phase == "directory-open" || phase == "directory-sync"
	if bytes.Equal(old, grantBytes(t, state, "authorized_keys")) == afterRename {
		t.Fatal("publication failure did not preserve its real rename boundary")
	}
	leftovers, err := filepath.Glob(filepath.Join(state, ".device-grants-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files=%v, error=%v", leftovers, err)
	}
}

func failGrantPublication(state, path string, next []byte, phase string, cause error) error {
	if phase == "stage" {
		return publishGrants(filepath.Join(state, "missing"), path, next)
	}
	if phase == "write" {
		root, err := os.OpenRoot(state)
		if err != nil {
			return grantFixtureError(err)
		}
		defer func() { _ = root.Close() }()
		file, err := root.OpenFile("read-only", os.O_CREATE|os.O_RDONLY, 0600)
		if err != nil {
			return grantFixtureError(err)
		}
		defer func() { _ = file.Close() }()
		return writeGrantPolicy(file, next, (*os.File).Sync)
	}
	if phase == "rename" {
		return publishGrants(state, filepath.Join(state, "missing", "policy"), next)
	}
	return publishGrantsWithSync(state, path, next, func(file *os.File) error {
		return failGrantSync(file, state, phase, cause)
	})
}

func failGrantSync(file *os.File, state, phase string, cause error) error {
	info, err := file.Stat()
	if err != nil {
		return grantFixtureError(err)
	}
	if phase == "file-sync" && !info.IsDir() || phase == "directory-sync" && info.IsDir() {
		return cause
	}
	if phase == "close" {
		return grantFixtureError(file.Close())
	}
	if phase == "directory-open" && !info.IsDir() {
		if err := file.Sync(); err != nil {
			return grantFixtureError(err)
		}
		root, err := os.OpenRoot(state)
		if err != nil {
			return grantFixtureError(err)
		}
		defer func() { _ = root.Close() }()
		return grantFixtureError(root.Chmod(".", 0300))
	}
	return grantFixtureError(file.Sync())
}

func TestGrantPublicationIgnoresUnrelatedCallerCancellation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		checkGrantLateCancellation(t, fail)
	}
}

func checkGrantLateCancellation(t *testing.T, fail bool) {
	t.Helper()
	state, id := t.TempDir(), grantActor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cause := errors.New("directory barrier failed")
	probe := new(grantPublicationProbe)
	if fail {
		probe.directoryError = cause
	}
	changed, err := changeDeviceWithPublisher(t.Context(), state, id, true, func(state, path string, next []byte) error {
		cancel()
		return probe.publish(state, path, next)
	})
	if !errors.Is(ctx.Err(), context.Canceled) || changed != !fail || (fail && !errors.Is(err, cause)) || (!fail && err != nil) {
		t.Fatalf("changed=%v, error=%v, context=%v", changed, err, ctx.Err())
	}
	if !GrantedIdentity(state, id) || probe.directories != 1 {
		t.Fatal("late cancellation abandoned publication")
	}
}

func grantFixtureRoot(t *testing.T, state string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func grantFixtureError(err error) error {
	if err != nil {
		return fmt.Errorf("grant fixture: %w", err)
	}
	return nil
}
