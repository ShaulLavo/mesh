package hostprofile

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fleetStoreFixture struct {
	store *FleetStore
	scope FleetScope
	id    string
	key   ed25519.PrivateKey
	at    time.Time
}

func newFleetFixture(t *testing.T) fleetStoreFixture {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(public)
	store := OpenFleetStore(t.TempDir())
	at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return at }
	return fleetStoreFixture{store: store, scope: FleetScope{Members: []string{id}, Trusted: func(string) bool { return true }}, id: id, key: key, at: at}
}
func (f fleetStoreFixture) doc(t *testing.T, revision uint64, text string) FleetDocument {
	t.Helper()
	doc, err := SignFleet(Editable{SchemaVersion: 1, Instructions: text}, "test", revision, f.at, f.id, f.key)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
func mustFleetWrite(t *testing.T, f fleetStoreFixture, expected uint64, doc FleetDocument) FleetWrite {
	t.Helper()
	result, err := f.store.WriteLocal(context.Background(), expected, doc, f.scope)
	if err != nil || !result.Complete() {
		t.Fatalf("write %+v: %v", result, err)
	}
	return result
}
func TestFleetTrustBeforeEveryComparison(t *testing.T) {
	f := newFleetFixture(t)
	doc := f.doc(t, 1, "one")
	mustFleetWrite(t, f, 0, doc)
	invalid := doc
	invalid.Signature = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if _, err := f.store.Receive(context.Background(), invalid, "", f.scope); err == nil {
		t.Fatal("same-content bad signature acknowledged")
	}
	revoked := f.scope
	revoked.Trusted = func(string) bool { return false }
	result, err := f.store.Receive(context.Background(), doc, "", revoked)
	if err != nil || result.Outcome != Untrusted {
		t.Fatal(result, err)
	}
	if _, err := f.store.WriteLocal(context.Background(), 0, doc, revoked); err == nil {
		t.Fatal("revoked local retry succeeded")
	}
	trustedOther := f.scope
	trustedOther.Trusted = func(id string) bool { return id != doc.Author }
	other := newFleetFixture(t)
	next := other.doc(t, 2, "replacement")
	result, err = f.store.Receive(context.Background(), next, "", trustedOther)
	if err != nil || result.Outcome != Applied {
		t.Fatal(result, err)
	}
	result, err = f.store.Receive(context.Background(), other.doc(t, 1, "lower"), "", trustedOther)
	if err != nil || result.Outcome != Older || result.Current == nil {
		t.Fatal(result, err)
	}
}
func TestFleetPublicationBarrierRetries(t *testing.T) {
	faults := []string{"parent", "replica-write", "replica-file-sync", "replica-rename", "replica-dir-sync", "outbox-write", "outbox-file-sync", "outbox-rename", "outbox-dir-sync"}
	for _, fault := range faults {
		t.Run(fault, func(t *testing.T) { testFleetBarrier(t, fault) })
	}
}
func testFleetBarrier(t *testing.T, fault string) {
	f := newFleetFixture(t)
	doc := f.doc(t, 1, "one")
	originalSync, originalWrite, originalFile, originalRename := f.store.syncDir, f.store.write, f.store.syncFile, f.store.rename
	inject := errors.New("injected " + fault)
	active := true
	target := ""
	f.store.rename = func(dir *os.File, from, to string) error {
		target = to
		if active && fault == "replica-rename" && to == "profile.json" {
			return inject
		}
		if active && fault == "outbox-rename" && to == "outbox.json" {
			return inject
		}
		return originalRename(dir, from, to)
	}
	writes := 0
	f.store.write = func(file *os.File, data []byte) error {
		writes++
		if active && ((fault == "replica-write" && writes == 1) || (fault == "outbox-write" && writes == 2)) {
			return syscall.ENOSPC
		}
		return originalWrite(file, data)
	}
	syncs := 0
	f.store.syncFile = func(file *os.File) error {
		syncs++
		if active && ((fault == "replica-file-sync" && syncs == 1) || (fault == "outbox-file-sync" && syncs == 2)) {
			return inject
		}
		return originalFile(file)
	}
	f.store.syncDir = func(dir *os.File) error {
		if active && fault == "parent" && dir.Name() != "fleet" {
			return inject
		}
		if active && ((fault == "replica-dir-sync" && target == "profile.json" && dir.Name() == "fleet") || (fault == "outbox-dir-sync" && target == "outbox.json")) {
			return inject
		}
		return originalSync(dir)
	}
	result, err := f.store.WriteLocal(context.Background(), 0, doc, f.scope)
	if err == nil {
		t.Fatalf("fault %s ignored: %+v", fault, result)
	}
	expected := BeforeReplicaRename
	if fault == "replica-dir-sync" {
		expected = VisibleUnconfirmed
	}
	if len(fault) >= 6 && fault[:6] == "outbox" {
		expected = ReplicaDurable
	}
	if result.Publication != expected || result.Complete() {
		t.Fatalf("wrong barrier stage: %+v %v", result, err)
	}
	active = false
	// A fresh store models a process restart. The exact N+1 retry repairs both barriers.
	f.store = OpenFleetStore(f.store.dir)
	retry := mustFleetWrite(t, f, 0, doc)
	if retry.Revision != 1 || len(retry.Outbox.Members) != 1 {
		t.Fatal(retry)
	}
	again := mustFleetWrite(t, f, 0, doc)
	if again.Changed || again.Revision != 1 {
		t.Fatal(again)
	}
}
func TestFleetRepeatedOutboxRepairCannotAcknowledge(t *testing.T) {
	f := newFleetFixture(t)
	doc := f.doc(t, 1, "one")
	rename := f.store.rename
	f.store.rename = func(dir *os.File, from, to string) error {
		if to == "outbox.json" {
			return errors.New("outbox blocked")
		}
		return rename(dir, from, to)
	}
	for range 3 {
		result, err := f.store.WriteLocal(context.Background(), 0, doc, f.scope)
		if err == nil || result.Publication != ReplicaDurable || result.Revision != 1 {
			t.Fatal(result, err)
		}
	}
	f.store.rename = rename
	result := mustFleetWrite(t, f, 0, doc)
	if result.Changed || result.Revision != 1 {
		t.Fatal(result)
	}
}
func TestFleetCASAndDistributedDivergence(t *testing.T) {
	f := newFleetFixture(t)
	docs := []FleetDocument{f.doc(t, 1, "one"), f.doc(t, 1, "two")}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, doc := range docs {
		wg.Go(func() { _, err := f.store.WriteLocal(context.Background(), 0, doc, f.scope); results <- err })
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
			continue
		}
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatal(err)
		}
		conflicts++
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	other := newFleetFixture(t)
	mustFleetWrite(t, other, 0, docs[1])
	result, err := other.store.Receive(context.Background(), docs[0], "", other.scope)
	if err != nil || result.Outcome != Conflict {
		t.Fatal(result, err)
	}
	result, err = other.store.Receive(context.Background(), f.doc(t, 2, "resolve"), "", other.scope)
	if err != nil || result.Outcome != Applied {
		t.Fatal(result, err)
	}
}
func TestFleetLateSettlementAndInvalidCurrent(t *testing.T) {
	f := newFleetFixture(t)
	one := f.doc(t, 1, "one")
	first := mustFleetWrite(t, f, 0, one)
	two := f.doc(t, 2, "two")
	bad := two
	bad.Signature = "bad"
	outcome := Outcome{Result: &PutResult{Outcome: Older, Revision: 2, Digest: bad.Digest(), Current: &bad}}
	if _, err := f.store.Settle(context.Background(), one.Digest(), f.id, outcome, f.scope); err == nil {
		t.Fatal("invalid Current adopted")
	}
	mustFleetWrite(t, f, 1, two)
	late := f.doc(t, 3, "late")
	outcome.Result = &PutResult{Outcome: Older, Revision: 3, Digest: late.Digest(), Current: &late}
	if _, err := f.store.Settle(context.Background(), first.Digest, f.id, outcome, f.scope); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := f.store.Read(f.scope.Trusted)
	if err != nil || snapshot.Value.Digest() != two.Digest() {
		t.Fatal(snapshot, err)
	}
	outcome.Result = &PutResult{Outcome: Older, Revision: 3, Digest: late.Digest(), Current: &late}
	if _, err := f.store.Settle(context.Background(), two.Digest(), f.id, outcome, f.scope); err != nil {
		t.Fatal(err)
	}
	snapshot, out, err := f.store.Read(f.scope.Trusted)
	if err != nil || snapshot.Value.Revision != 3 || out.Members[0].State != Delivered {
		t.Fatal(snapshot, out, err)
	}
}
func TestFleetDamagedCurrentRevisionFloor(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			f := newFleetFixture(t)
			doc := f.doc(t, 5, "floor")
			directory := filepath.Join(f.store.dir, "fleet")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			doc.Signature = "damaged"
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if !known {
				data = []byte(`{"revision":`)
			}
			if err := os.WriteFile(filepath.Join(directory, "profile.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.store.ReadReplica(f.scope.Trusted)
			if err != nil || snapshot.State != StateInvalid || (snapshot.Revision != nil) != known {
				t.Fatal(snapshot, err)
			}
			if _, err := f.store.Receive(context.Background(), f.doc(t, 4, "lower"), "", f.scope); err == nil {
				t.Fatal("damaged floor rolled back")
			}
			result, err := f.store.Receive(context.Background(), f.doc(t, 6, "repair"), "", f.scope)
			if known && (err != nil || result.Outcome != Applied) {
				t.Fatal(result, err)
			}
			if !known && err == nil {
				t.Fatal("unknown revision silently reset")
			}
		})
	}
}
func TestFleetGiveUpOnlyAfterFailureAndExplicitRetry(t *testing.T) {
	f := newFleetFixture(t)
	one := f.doc(t, 1, "one")
	mustFleetWrite(t, f, 0, one)
	f.store.now = func() time.Time { return f.at.Add(FleetGiveUp + time.Hour) }
	result, err := f.store.Reconcile(context.Background(), f.scope)
	if err != nil || result.Outbox.Members[0].State != Pending {
		t.Fatal(result, err)
	}
	failure := Outcome{Error: &DeliveryError{Code: "host.unreachable", Message: "offline", Retryable: true}}
	result, err = f.store.Settle(context.Background(), one.Digest(), f.id, failure, f.scope)
	if err != nil || result.Outbox.Members[0].State != Stale {
		t.Fatal(result, err)
	}
	result, err = f.store.Requeue(context.Background(), f.scope)
	if err != nil || result.Outbox.Members[0].State != Pending || !result.Outbox.Members[0].QueuedAt.Equal(f.store.now()) {
		t.Fatal(result, err)
	}
	result, err = f.store.Settle(context.Background(), one.Digest(), f.id, failure, f.scope)
	if err != nil || result.Outbox.Members[0].State != Pending {
		t.Fatal(result, err)
	}
}

func TestFleetIdenticalConcurrentCASPublishesOnce(t *testing.T) {
	f := newFleetFixture(t)
	doc := f.doc(t, 1, "same")
	results := make(chan FleetWrite, 2)
	errs := make(chan error, 2)
	var joined sync.WaitGroup
	for range 2 {
		joined.Go(func() {
			result, err := f.store.WriteLocal(t.Context(), 0, doc, f.scope)
			results <- result
			errs <- err
		})
	}
	joined.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := 0
	for result := range results {
		if result.Changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatal(changed)
	}
}
func TestFleetEveryMutationRepairsRetryBarriers(t *testing.T) {
	for _, operation := range []string{"write", "receive", "reconcile", "requeue", "settle"} {
		t.Run(operation, func(t *testing.T) { testFleetMutationRetryBarrier(t, operation) })
	}
}
func testFleetMutationRetryBarrier(t *testing.T, operation string) {
	f := newFleetFixture(t)
	doc := f.doc(t, 1, "same")
	mustFleetWrite(t, f, 0, doc)
	barrier := f.store.syncDir
	broken := true
	f.store.syncDir = func(dir *os.File) error {
		if broken && dir.Name() == "fleet" {
			return syscall.EIO
		}
		return barrier(dir)
	}
	call := func() error {
		switch operation {
		case "write":
			_, err := f.store.WriteLocal(t.Context(), 0, doc, f.scope)
			return err
		case "receive":
			_, err := f.store.Receive(t.Context(), doc, "", f.scope)
			return err
		case "reconcile":
			_, err := f.store.Reconcile(t.Context(), f.scope)
			return err
		case "requeue":
			_, err := f.store.Requeue(t.Context(), f.scope)
			return err
		case "settle":
			_, err := f.store.Settle(t.Context(), doc.Digest(), f.id, Outcome{Result: &PutResult{Outcome: Unchanged, Revision: 1, Digest: doc.Digest()}}, f.scope)
			return err
		default:
			return errors.New("unknown test operation")
		}
	}
	for range 2 {
		if err := call(); !errors.Is(err, syscall.EIO) {
			t.Fatalf("unchanged bypassed %s barrier: %v", operation, err)
		}
	}
	broken = false
	if err := call(); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := f.store.Read(f.scope.Trusted)
	if err != nil || snapshot.Value.Revision != 1 {
		t.Fatal(snapshot, err)
	}
}
func TestFleetReceiveTableAndOutboxReconstruction(t *testing.T) {
	f := newFleetFixture(t)
	doc := f.doc(t, 2, "two")
	result, err := f.store.Receive(t.Context(), doc, f.id, f.scope)
	if err != nil || result.Outcome != Applied {
		t.Fatal(result, err)
	}
	_, out, err := f.store.Read(f.scope.Trusted)
	if err != nil || out.Members[0].State != Delivered {
		t.Fatal(out, err)
	}
	for _, test := range []struct {
		doc  FleetDocument
		want Acceptance
	}{
		{doc, Unchanged}, {f.doc(t, 1, "old"), Older}, {f.doc(t, 2, "diverged"), Conflict},
	} {
		result, err := f.store.Receive(t.Context(), test.doc, "", f.scope)
		if err != nil || result.Outcome != test.want {
			t.Fatal(result, err)
		}
	}
	originalAge := out.Members[0].QueuedAt
	if err := os.Remove(filepath.Join(f.store.dir, "fleet", "outbox.json")); err != nil {
		t.Fatal(err)
	}
	f.store.now = func() time.Time { return f.at.Add(time.Hour) }
	repaired, err := f.store.Reconcile(t.Context(), f.scope)
	if err != nil || repaired.Outbox.Members[0].State != Pending || !repaired.Outbox.Members[0].QueuedAt.After(originalAge) {
		t.Fatal(repaired, err)
	}
	persistedAge := repaired.Outbox.Members[0].QueuedAt
	f.store.now = func() time.Time { return f.at.Add(2 * time.Hour) }
	repaired, err = f.store.Reconcile(t.Context(), f.scope)
	if err != nil || !repaired.Outbox.Members[0].QueuedAt.Equal(persistedAge) {
		t.Fatal(repaired, err)
	}
}
func TestFleetFailureBeforeRenamePreservesAuthoritativeReplica(t *testing.T) {
	f := newFleetFixture(t)
	one := f.doc(t, 1, "one")
	mustFleetWrite(t, f, 0, one)
	f.store.rename = func(*os.File, string, string) error { return syscall.ENOSPC }
	result, err := f.store.WriteLocal(t.Context(), 1, f.doc(t, 2, "two"), f.scope)
	if !errors.Is(err, syscall.ENOSPC) || result.Publication != BeforeReplicaRename {
		t.Fatal(result, err)
	}
	snapshot, err := f.store.ReadReplica(f.scope.Trusted)
	if err != nil || snapshot.Value.Digest() != one.Digest() {
		t.Fatal(snapshot, err)
	}
}
func TestFleetCanceledStoreOperationDoesNotPublish(t *testing.T) {
	f := newFleetFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.store.WriteLocal(ctx, 0, f.doc(t, 1, "one"), f.scope); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.store.dir, "fleet")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled write created fleet", err)
	}
}

func TestFleetRevokedCurrentStillSetsRevisionFloor(t *testing.T) {
	f := newFleetFixture(t)
	mustFleetWrite(t, f, 0, f.doc(t, 1, "revoked"))
	replacement := newFleetFixture(t)
	scope := f.scope
	scope.Trusted = func(id string) bool { return id == replacement.id }
	lower := replacement.doc(t, 1, "cannot replace revoked at same revision")
	if _, err := f.store.Receive(t.Context(), lower, "", scope); err == nil {
		t.Fatal("revoked author's revision floor was discarded")
	}
	higher := replacement.doc(t, 2, "trusted replacement")
	result, err := f.store.Receive(t.Context(), higher, "", scope)
	if err != nil || result.Outcome != Applied {
		t.Fatal(result, err)
	}
}

func TestFleetPutResultStrictCurrentDecoding(t *testing.T) {
	f := newFleetFixture(t)
	current := f.doc(t, 2, "two")
	data, err := json.Marshal(PutResult{Outcome: Older, Revision: 2, Digest: current.Digest(), Current: &current})
	if err != nil {
		t.Fatal(err)
	}
	var result PutResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(f.doc(t, 1, "one")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		bytes.Replace(data, []byte(`"current":{`), []byte(`"current":{"unknown":true,`), 1),
		bytes.Replace(data, []byte(`"current":{`), []byte(`"current":{"revision":8,`), 1),
		bytes.Replace(data, []byte(`"outcome":"older"`), []byte(`"outcome":"older","Outcome":"applied"`), 1),
	} {
		if err := json.Unmarshal(bad, &result); err == nil {
			t.Fatalf("accepted non-strict Current: %s", bad)
		}
	}
}
