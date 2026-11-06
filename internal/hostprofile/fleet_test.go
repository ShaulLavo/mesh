package hostprofile

import (
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
