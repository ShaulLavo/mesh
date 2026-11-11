package update

import (
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func committedAdoptionJournal(t *testing.T) (*hostFixture, updateinstall.Status, HostAdoption) {
	t.Helper()
	f := newHostFixture(t)
	request := updateinstall.Request{ID: strings.Repeat("a", 32), Generation: 1, TargetID: "host", Current: f.current, Manifest: f.manifests["v0.1.2"]}
	if _, err := f.engine.Stage(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Grant(t.Context(), request.ID, request.Generation); err != nil {
		t.Fatal(err)
	}
	f.finish()
	status := f.installation()
	if _, err := f.store.SetHostPolicy(t.Context(), 1, HostPolicy{Mode: HostManual}); err != nil {
		t.Fatal(err)
	}
	pin := discoveryDigest(f.files[request.Manifest.Version+"/"+release.UpdateMetadataAssetName])
	envelope := HostAdoption{Schema: 1, Operation: request.ID, Pin: pin,
		Details: mustHostJSON(map[string]any{"schema": 1, "id": request.ID, "pin": pin, "request": request, "settings": status.Settings, "phase": "confirming", "future": map[string]any{"preserve": true}})}
	if err := f.store.writeAuthority("host-adoption.json", envelope); err != nil {
		t.Fatal(err)
	}
	return f, status, envelope
}

func TestHostAdoptionReconcileYieldsBeforeManualLedgerDedup(t *testing.T) {
	f, journal, envelope := committedAdoptionJournal(t)
	f.tick()
	f.tick()
	entries, _, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unconfirmed adoption became manual history: %+v %v", entries, err)
	}
	state, err := f.store.HostState(t.Context())
	if err != nil || state.LastRequest == journal.Request.ID || state.Phase != HostWaiting {
		t.Fatalf("confirmation was deduplicated before publication: %+v %v", state, err)
	}
	entry := LedgerEntry{Request: journal.Request.ID, Generation: journal.Request.Generation, Version: journal.Request.Manifest.Version,
		Metadata: envelope.Pin, Binary: journal.Verified.Build.Digest, From: journal.Request.Current.Digest, Origin: "adoption", CommittedAt: journal.UpdatedAt, Workers: 1, WorkersPreserved: 1}
	unlock, err := f.store.lockContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.appendLedger(entry)
	if err == nil {
		envelope.Confirmed, envelope.Adopted = true, &entry
		err = f.store.writeAuthority("host-adoption.json", envelope)
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	f.updater = f.newUpdater()
	f.tick()
	f.tick()
	entries, _, err = f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 1 || entries[0].Origin != "adoption" {
		t.Fatalf("confirmation lost adoption origin: %+v %v", entries, err)
	}
}

func TestHostAdoptionReconcileKeepsForeignJournalHistory(t *testing.T) {
	f, _, envelope := committedAdoptionJournal(t)
	envelope.Operation = strings.Repeat("b", 32)
	if err := f.store.writeAuthority("host-adoption.json", envelope); err != nil {
		t.Fatal(err)
	}
	f.tick()
	entries, _, err := f.store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 1 || entries[0].Origin != string(HostManual) || entries[0].Metadata != "" {
		t.Fatalf("foreign manual journal was claimed by adoption: %+v %v", entries, err)
	}
}
