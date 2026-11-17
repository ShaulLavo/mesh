package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

type adoptionRemoteFixture struct {
	*adoptionFixture
	aliases   []string
	platform  release.Platform
	probeErr  error
	calls     int
	deploys   []AdoptionRemote
	deployErr error
}

// The remote mirrors the Mac in the November 17 fleet: a member whose fleet
// record has no platform, reached over the system SSH alias "shaul-mac".
func newAdoptionRemoteFixture(t *testing.T) *adoptionRemoteFixture {
	t.Helper()
	f := &adoptionRemoteFixture{adoptionFixture: newAdoptionFixture(t), platform: release.Platform{OS: "darwin", Arch: "arm64"}}
	remote := Host{ID: updateTestID(t), Endpoint: "wss://fixture.invalid/mesh"}
	f.start.Require = []Host{remote}
	f.start.Remotes = []AdoptionRemote{{Host: remote, SSH: "shaul-mac", Policy: HostPolicy{Schema: 1, Revision: 1, Mode: HostAutomatic}, State: AdoptionRemotePending}}
	f.start.Notice = &AdoptionNotice{Command: []string{"true"}, Lead: 30 * time.Minute}
	f.supervisor.RemotePlatform = func(_ context.Context, alias string) (release.Platform, error) {
		f.aliases = append(f.aliases, alias)
		return f.platform, f.probeErr
	}
	f.supervisor.Deploy = func(ctx context.Context, operation Adoption, remote AdoptionRemote) error {
		f.calls++
		// Production SSH starts here, so the durable record must already say a
		// deploy began; otherwise a crash would leave the remote "pending".
		if _, stored, err := f.base.store.AdoptionStatus(ctx); err != nil || stored.Remotes[0].State != AdoptionRemoteDeploying {
			f.base.t.Errorf("deploy started before %q was recorded: %v", AdoptionRemoteDeploying, err)
		}
		if _, err := AdoptionRemoteCommand(operation, remote); err != nil {
			return err
		}
		f.deploys = append(f.deploys, remote)
		return f.deployErr
	}
	return f
}

func (f *adoptionRemoteFixture) adoptionPath() string {
	return filepath.Join(f.base.store.stateDir(), "updates", "host-adoption.json")
}

func TestAdoptionFillsMissingRemotePlatformOverSSH(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	_, operation := f.state(t)
	if !reflect.DeepEqual(f.aliases, []string{"shaul-mac"}) {
		t.Fatalf("platform probe aliases = %v", f.aliases)
	}
	if operation.Remotes[0].Host.Platform != f.platform || operation.Require[0].Platform != f.platform {
		t.Fatalf("recorded remote platform = %+v, predecessor %+v", operation.Remotes[0].Host.Platform, operation.Require[0].Platform)
	}
	f.base.now = f.base.now.Add(time.Minute)
	if err := f.supervisor.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.deploys) != 1 || f.deploys[0].Host.Platform != f.platform {
		t.Fatalf("deploys = %+v", f.deploys)
	}
	if _, operation = f.state(t); operation.Remotes[0].State != AdoptionRemoteStarted {
		t.Fatalf("remote state = %s: %s", operation.Remotes[0].State, operation.Remotes[0].Problem)
	}
}

func TestAdoptionProbesARemoteWhosePlatformIsKnown(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	f.start.Remotes[0].Host.Platform = f.platform
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	if _, operation := f.state(t); !reflect.DeepEqual(f.aliases, []string{"shaul-mac"}) || operation.Remotes[0].Host.Platform != f.platform {
		t.Fatalf("known platform was not checked over SSH or changed: %v %+v", f.aliases, operation.Remotes[0].Host)
	}
}

func TestAdoptionRejectsUnknownRemotePlatformBeforeRecording(t *testing.T) {
	cases := map[string]func(*adoptionRemoteFixture){
		"probe fails": func(f *adoptionRemoteFixture) { f.probeErr = errors.New("ssh: connect timed out") },
		"deploy cache missing": func(f *adoptionRemoteFixture) {
			f.probeErr = errors.New("existing daemon update cache /Users/shaul/Library/Caches/mesh/updates is unavailable")
		},
		"no probe":    func(f *adoptionRemoteFixture) { f.supervisor.RemotePlatform = nil },
		"partial":     func(f *adoptionRemoteFixture) { f.platform = release.Platform{OS: "linux"} },
		"no artifact": func(f *adoptionRemoteFixture) { f.platform = release.Platform{OS: "darwin", Arch: "amd64"} },
		"fleet differs": func(f *adoptionRemoteFixture) {
			f.start.Remotes[0].Host.Platform = release.Platform{OS: "linux", Arch: "arm64"}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAdoptionRemoteFixture(t)
			change(f)
			if err := f.supervisor.Start(t.Context(), f.start); err == nil {
				t.Fatal("remote without a usable platform was accepted")
			}
			if _, err := os.Stat(f.adoptionPath()); !errors.Is(err, os.ErrNotExist) || f.promotions != 0 {
				t.Fatalf("rejected adoption left a record or promoted the helper: %v", err)
			}
		})
	}
}

func TestAdoptionRemoteCommandRefusesEmptyPlatform(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	_, operation := f.state(t)
	remote := operation.Remotes[0]
	remote.Host.Platform = release.Platform{}
	_, err := AdoptionRemoteCommand(operation, remote)
	if err == nil || !strings.Contains(err.Error(), "no recorded platform") || strings.Contains(err.Error(), "for /") {
		t.Fatalf("empty platform command error = %v", err)
	}
}

// stuckAdoption rewrites the record into the state STOPPED.md describes for
// operation 0a5d5ea5: release v0.1.329 recorded every remote with the empty
// platform its fleet member had, promoted the PC helper, then blocked each
// deploy at the artifact lookup for "/" before any SSH connection.
func stuckAdoption(t *testing.T, f *adoptionRemoteFixture) Adoption {
	t.Helper()
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	_, recorded := f.state(t)
	stuck := recorded
	stuck.Require = slices.Clone(recorded.Require)
	stuck.Require[0].Platform = release.Platform{}
	stuck.Remotes = slices.Clone(recorded.Remotes)
	remote := &stuck.Remotes[0]
	remote.Host.Platform = release.Platform{}
	remote.Require = nil
	remote.State, remote.Problem, remote.NextAttempt = AdoptionRemoteBlocked, "host updater: release: manifest v0.1.3 has no artifact for /", f.base.now.Add(time.Minute)
	stuck.Problem, stuck.Failures = "update: predecessor "+remote.Host.ID+": update evidence unsupported", 1
	if err := f.base.store.saveAdoption(t.Context(), recorded, stuck); err != nil {
		t.Fatal(err)
	}
	return stuck
}

func TestAbandonStuckAdoptionFromRolloutStop(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	stuck := stuckAdoption(t, f)
	if stuck.Phase != AdoptionWaiting || stuck.Helper == nil {
		t.Fatalf("fixture is not the stuck waiting adoption: %+v", stuck)
	}
	// The minute retry keeps failing before SSH and leaves the remote untouched.
	f.base.now = f.base.now.Add(2 * time.Minute)
	if err := f.supervisor.Resume(t.Context()); err == nil || !strings.Contains(err.Error(), "no recorded platform") {
		t.Fatalf("stuck retry = %v", err)
	}
	if f.calls != 0 {
		t.Fatalf("empty platform reached the deploy hook %d times", f.calls)
	}
	_, retried := f.state(t)
	restore := func(context.Context, Adoption) (bool, error) {
		t.Fatal("journaled adoption restored its helper")
		return false, nil
	}
	record, err := AbandonAdoption(t.Context(), f.base.store, restore, f.base.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.adoptionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandon kept the adoption hold: %v", err)
	}
	policy, err := f.base.store.HostPolicy(t.Context())
	want := HostPolicy{Schema: 1, Revision: retried.Policy.Revision + 1, Mode: HostManual}
	if err != nil || !reflect.DeepEqual(policy, want) || !record.PolicyWithdrawn || !reflect.DeepEqual(record.Policy, want) {
		t.Fatalf("policy after abandon = %+v %v, record %+v", policy, err, record)
	}
	if record.Operation != retried.ID || record.Phase != AdoptionWaiting || record.HelperRestored || record.Helper == nil || *record.Helper != *retried.Helper {
		t.Fatalf("abandonment record = %+v", record)
	}
	var stored AdoptionAbandonment
	if err := f.base.store.readAuthority("adoption-abandoned-"+retried.ID+".json", &stored); err != nil || stored.Adoption.Operation != retried.ID {
		t.Fatalf("durable abandonment record = %+v %v", stored, err)
	}
	if pending, err := AdoptionPending(t.Context(), f.base.store.stateDir()); err != nil || pending {
		t.Fatalf("abandoned adoption still fences startup: %v %v", pending, err)
	}
	if _, err := AbandonAdoption(t.Context(), f.base.store, restore, f.base.now); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second abandon = %v", err)
	}
	// The owner can start again; the fresh operation resolves the platform.
	f.start.ID = strings.Repeat("b", 32)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatalf("restart after abandon: %v", err)
	}
	if _, next := f.state(t); next.ID != f.start.ID || next.Remotes[0].Host.Platform != f.platform || next.Policy.Revision != want.Revision+1 {
		t.Fatalf("restarted adoption = %+v", next)
	}
}

func TestAbandonRefusesTouchedAdoption(t *testing.T) {
	cases := map[string]func(*testing.T, *adoptionRemoteFixture){
		"deploy failed after SSH": func(t *testing.T, f *adoptionRemoteFixture) {
			f.deployErr = errors.New("run system SSH command: exit status 1")
			f.resumeAfterRetry(t)
		},
		"deploy started": func(t *testing.T, f *adoptionRemoteFixture) {
			f.resumeAfterRetry(t)
		},
		"deploy interrupted": func(t *testing.T, f *adoptionRemoteFixture) {
			f.change(t, func(next *Adoption) { next.Remotes[0].State = AdoptionRemoteDeploying })
		},
		"notice delivered": func(t *testing.T, f *adoptionRemoteFixture) {
			f.change(t, func(next *Adoption) { next.Notice.DeliveredAt = f.base.now })
		},
		"activating": func(t *testing.T, f *adoptionRemoteFixture) {
			f.change(t, func(next *Adoption) { next.Phase = AdoptionActivating })
		},
	}
	for name, touch := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAdoptionRemoteFixture(t)
			if err := f.supervisor.Start(t.Context(), f.start); err != nil {
				t.Fatal(err)
			}
			touch(t, f)
			before, err := os.ReadFile(f.adoptionPath())
			if err != nil {
				t.Fatal(err)
			}
			policy, err := f.base.store.HostPolicy(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AbandonAdoption(t.Context(), f.base.store, nil, f.base.now); err == nil {
				t.Fatal("touched adoption was abandoned")
			}
			after, err := os.ReadFile(f.adoptionPath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refused abandon changed the adoption: %v", err)
			}
			if again, err := f.base.store.HostPolicy(t.Context()); err != nil || !reflect.DeepEqual(again, policy) {
				t.Fatalf("refused abandon changed the policy: %+v %v", again, err)
			}
		})
	}
}

// A helper killed during SSH leaves exactly the record that existed while the
// deploy ran. That record must already refuse abandon.
func TestAdoptionCrashDuringDeployLeavesRemoteTouched(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	if err := f.supervisor.Start(t.Context(), f.start); err != nil {
		t.Fatal(err)
	}
	var during error
	f.supervisor.Deploy = func(ctx context.Context, _ Adoption, _ AdoptionRemote) error {
		envelope, stored, err := f.base.store.AdoptionStatus(ctx)
		if err != nil {
			return err
		}
		during = checkAdoptionAbandonable(envelope, stored)
		return errors.New("helper killed during SSH")
	}
	f.base.now = f.base.now.Add(time.Minute)
	if err := f.supervisor.Resume(t.Context()); err == nil {
		t.Fatal("killed deploy reported success")
	}
	if during == nil {
		t.Fatal("the record written before SSH still allowed abandon")
	}
}

func (f *adoptionRemoteFixture) resumeAfterRetry(t *testing.T) {
	t.Helper()
	f.base.now = f.base.now.Add(time.Minute)
	_ = f.supervisor.Resume(t.Context())
	if len(f.deploys) != 1 {
		t.Fatalf("deploy did not run: %+v", f.deploys)
	}
}

func (f *adoptionRemoteFixture) change(t *testing.T, apply func(*Adoption)) {
	t.Helper()
	_, operation := f.state(t)
	next := operation
	next.Remotes = slices.Clone(operation.Remotes)
	notice := *operation.Notice
	next.Notice = &notice
	apply(&next)
	if err := f.base.store.saveAdoption(t.Context(), operation, next); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonRestoresUnjournaledInitialHelper(t *testing.T) {
	f := newAdoptionRemoteFixture(t)
	stuck := stuckAdoption(t, f)
	f.change(t, func(next *Adoption) { next.Unjournaled = true })
	failed := func(context.Context, Adoption) (bool, error) { return false, errors.New("helper manager refused") }
	if _, err := AbandonAdoption(t.Context(), f.base.store, failed, f.base.now); err == nil {
		t.Fatal("abandon ignored a failed helper restore")
	}
	if _, err := os.Stat(f.adoptionPath()); err != nil {
		t.Fatalf("failed restore dropped the adoption hold: %v", err)
	}
	var restored []string
	restore := func(_ context.Context, operation Adoption) (bool, error) {
		restored = append(restored, operation.ID)
		return true, nil
	}
	record, err := AbandonAdoption(t.Context(), f.base.store, restore, f.base.now)
	if err != nil || !record.HelperRestored || record.Helper != nil || !reflect.DeepEqual(restored, []string{stuck.ID}) {
		t.Fatalf("unjournaled abandon = %+v %v, restored %v", record, err, restored)
	}
}
