package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ownerA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ownerB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func openKey(owner, opaque string) Key {
	return Key{Host: "host", Class: ApplicationClass, Principal: owner, Opaque: opaque, Operation: OperationContextOpen}
}
func jobKey(owner, context, opaque string) Key {
	return Key{Host: "host", Class: ApplicationClass, Principal: owner, Opaque: opaque, Context: context, Operation: OperationExec}
}

func contextJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(t.TempDir(), "boot", "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func fixedSpec(spec ContextSpec) func() (ContextSpec, error) {
	return func() (ContextSpec, error) { return spec, nil }
}

func openSpec(t *testing.T, j *Journal, owner, key string, spec ContextSpec) Context {
	t.Helper()
	c, created, err := j.OpenContext(t.Context(), openKey(owner, key), Digest(spec.Cwd), fixedSpec(spec))
	if err != nil || !created {
		t.Fatalf("open %s: created=%v err=%v", key, created, err)
	}
	return c
}

func openTestContext(t *testing.T, j *Journal, owner, key, cwd string) Context {
	t.Helper()
	return openSpec(t, j, owner, key, ContextSpec{Cwd: cwd, Shell: "sh", ShellPath: "/bin/sh"})
}

// reserveJob reserves a pipe job whose argv is fixed and whose cwd and
// overlay come from the context revision it sees.
func reserveJob(t *testing.T, j *Journal, k Key, request string, seen func(Context)) (Receipt, bool, error) {
	t.Helper()
	resolve := func(c Context) (Intent, map[string]string, error) {
		if seen != nil {
			seen(c)
		}
		return Intent{Kind: KindPipe, Command: []string{"/bin/echo", request}, Cwd: c.Cwd, Stdin: StdinClosed}, c.Env, nil
	}
	return j.ReserveInContext(t.Context(), k, Digest(request), resolve, func(s LaunchSnapshot) (Binding, error) {
		return newTestBinding(t, j, k, s.Intent, []string{"PATH=/bin"})
	})
}

func TestContextOpenReplayReturnsOriginalResult(t *testing.T) {
	j := contextJournal(t)
	c := openSpec(t, j, ownerA, "ctx-1", ContextSpec{Cwd: "/work", Shell: "login", ShellPath: "/bin/zsh"})
	cwd := "/work/moved"
	if _, err := j.UpdateContext(t.Context(), ownerA, c.ID, 1, ContextChange{Cwd: &cwd}, func(ContextSpec) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The host changed its login shell and lost the directory: a retry must
	// not resolve again, and must return what the first open accepted.
	unresolvable := func() (ContextSpec, error) {
		t.Error("replay resolved host state")
		return ContextSpec{}, errors.New("gone")
	}
	again, created, err := j.OpenContext(t.Context(), openKey(ownerA, "ctx-1"), Digest("/work"), unresolvable)
	if err != nil || created || again.ID != c.ID || again.Revision != 1 || again.Cwd != "/work" || again.ShellPath != "/bin/zsh" {
		t.Fatalf("replay = %+v created=%v err=%v", again, created, err)
	}
	if _, _, err := j.OpenContext(t.Context(), openKey(ownerA, "ctx-1"), Digest("/other"), unresolvable); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed intent = %v", err)
	}
	if _, err := j.CloseContext(t.Context(), ownerA, c.ID, nil); err != nil {
		t.Fatal(err)
	}
	found, err := j.ContextReceipt(t.Context(), openKey(ownerA, "ctx-1"))
	if err != nil || found.ID != c.ID || found.Cwd != "/work" || found.Revision != 1 {
		t.Fatalf("receipt after edit and close = %+v, %v", found, err)
	}
	if _, err := j.ContextReceipt(t.Context(), openKey(ownerB, "ctx-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other caller's receipt lookup = %v", err)
	}
	other := openTestContext(t, j, ownerB, "ctx-1", "/work")
	if other.ID == c.ID {
		t.Fatal("two callers share one context")
	}
}

func TestContextRecordLostAfterReceiptIsCompleted(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/work")
	if err := j.files.Remove(contextName(c.ID), journalAnchor); err != nil {
		t.Fatal(err)
	}
	found, err := j.ContextReceipt(t.Context(), openKey(ownerA, "ctx-1"))
	if err != nil || found.ID != c.ID || found.Revision != 1 {
		t.Fatalf("receipt completion = %+v, %v", found, err)
	}
	if _, err := j.GetContext(t.Context(), ownerA, c.ID); err != nil {
		t.Fatalf("completed record unreadable: %v", err)
	}
}

func TestContextQuotaCountsInterruptedOpens(t *testing.T) {
	j := contextJournal(t)
	j.contextOwnerLimit = 3
	openTestContext(t, j, ownerA, "c-0", "/work")
	openTestContext(t, j, ownerA, "c-1", "/work")
	pending := openTestContext(t, j, ownerA, "c-2", "/work")
	// The record write was interrupted after the receipt became durable.
	if err := j.files.Remove(contextName(pending.ID), journalAnchor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.OpenContext(t.Context(), openKey(ownerA, "one-more"), Digest("x"), fixedSpec(ContextSpec{Cwd: "/work", Shell: "none"})); !errors.Is(err, ErrContextLimit) {
		t.Fatalf("open past an interrupted last slot = %v", err)
	}
	if _, err := j.ContextReceipt(t.Context(), openKey(ownerA, "c-2")); err != nil {
		t.Fatal(err)
	}
	list, _, err := j.ListContexts(t.Context(), ownerA, "", 100)
	if err != nil || len(list) != 3 {
		t.Fatalf("caller holds %d contexts, %v", len(list), err)
	}
}

func TestContextQuotaPerHostAndExpiredReclaim(t *testing.T) {
	j := contextJournal(t)
	j.contextHostLimit = 2
	openTestContext(t, j, ownerA, "a", "/work")
	openTestContext(t, j, ownerB, "b", "/work")
	if _, _, err := j.OpenContext(t.Context(), openKey(ownerB, "c"), Digest("x"), fixedSpec(ContextSpec{Cwd: "/work", Shell: "none"})); !errors.Is(err, ErrContextLimit) {
		t.Fatalf("host limit = %v", err)
	}
	later := time.Now().Add(31 * 24 * time.Hour)
	j.now = func() time.Time { return later }
	fresh := openTestContext(t, j, ownerB, "c", "/work")
	if list, _, _ := j.ListContexts(t.Context(), ownerA, "", 10); len(list) != 0 || fresh.ID == "" {
		t.Fatalf("expired slots were not reclaimed: %d left", len(list))
	}
}

func TestInterruptedExpiryNeverResurrects(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx", "/work")
	request, _, err := j.readRequest(openKey(ownerA, "ctx").Hash())
	if err != nil {
		t.Fatal(err)
	}
	// Expiry recorded its decision and removed the record, then stopped.
	request.Expired = true
	if _, err := j.files.PublishBounded(contextRequestName(request.Key.Hash()), request, false, contextRecordLimit); err != nil {
		t.Fatal(err)
	}
	if err := j.files.Remove(contextName(c.ID), journalAnchor); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ContextReceipt(t.Context(), openKey(ownerA, "ctx")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired receipt lookup = %v", err)
	}
	if _, err := j.GetContext(t.Context(), ownerA, c.ID); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("expired context came back: %v", err)
	}
	fresh := openTestContext(t, j, ownerA, "ctx", "/work")
	if fresh.ID == c.ID {
		t.Fatal("expired context reopened with its old ID")
	}
}

func TestContextOwnershipAndRevisions(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/work")
	if _, err := j.GetContext(t.Context(), ownerB, c.ID); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("other caller get = %v", err)
	}
	if _, err := j.UpdateContext(t.Context(), ownerB, c.ID, 1, ContextChange{}, func(ContextSpec) error { return nil }); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("other caller update = %v", err)
	}
	if _, err := j.CloseContext(t.Context(), ownerB, c.ID, nil); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("other caller close = %v", err)
	}
	if list, _, err := j.ListContexts(t.Context(), ownerB, "", 10); err != nil || len(list) != 0 {
		t.Fatalf("other caller list = %d, %v", len(list), err)
	}
	cwd := "/work/app"
	updated, err := j.UpdateContext(t.Context(), ownerA, c.ID, 1, ContextChange{Cwd: &cwd, SetEnv: map[string]string{"A": "1"}}, func(ContextSpec) error { return nil })
	if err != nil || updated.Revision != 2 || updated.Cwd != cwd || updated.Env["A"] != "1" {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := j.UpdateContext(t.Context(), ownerA, c.ID, 1, ContextChange{}, func(ContextSpec) error { return nil }); !errors.Is(err, ErrContextConflict) {
		t.Fatalf("stale update = %v", err)
	}
	stale := uint64(1)
	if _, err := j.CloseContext(t.Context(), ownerA, c.ID, &stale); !errors.Is(err, ErrContextConflict) {
		t.Fatalf("stale close = %v", err)
	}
	closed, err := j.CloseContext(t.Context(), ownerA, c.ID, nil)
	if err != nil || closed.ClosedAt == nil || closed.Revision != 3 {
		t.Fatalf("close = %+v, %v", closed, err)
	}
	if _, err := j.UpdateContext(t.Context(), ownerA, c.ID, 3, ContextChange{}, func(ContextSpec) error { return nil }); !errors.Is(err, ErrContextClosed) {
		t.Fatalf("update after close = %v", err)
	}
	if SameOwner("", "") || !SameOwner(ownerA, ownerA) || SameOwner(ownerA, ownerB) {
		t.Fatal("owner comparison")
	}
}

func TestContextAdmissionIsRecheckedBehindTheLock(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx", "/work")
	revoked := WithAdmission(t.Context(), func() error { return errors.New("grant revoked") })
	if _, _, err := j.OpenContext(revoked, openKey(ownerA, "new"), Digest("x"), fixedSpec(ContextSpec{Cwd: "/work", Shell: "none"})); err == nil {
		t.Fatal("open admitted after revocation")
	}
	if found, _, _ := j.readRequest(openKey(ownerA, "new").Hash()); found.ContextID != "" {
		t.Fatal("revoked open left a receipt")
	}
	cwd := "/elsewhere"
	if _, err := j.UpdateContext(revoked, ownerA, c.ID, 1, ContextChange{Cwd: &cwd}, func(ContextSpec) error { return nil }); err == nil {
		t.Fatal("update admitted after revocation")
	}
	if _, _, err := reserveJobWith(revoked, t, j, jobKey(ownerA, c.ID, "job")); err == nil {
		t.Fatal("job admitted after revocation")
	}
	after, err := j.GetContext(t.Context(), ownerA, c.ID)
	if err != nil || after.Revision != 1 || after.Cwd != "/work" || len(after.Members) != 0 {
		t.Fatalf("revoked requests changed the context: %+v %v", after, err)
	}
}

func reserveJobWith(ctx context.Context, t *testing.T, j *Journal, k Key) (Receipt, bool, error) {
	t.Helper()
	resolve := func(c Context) (Intent, map[string]string, error) {
		return Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: c.Cwd, Stdin: StdinClosed}, nil, nil
	}
	return j.ReserveInContext(ctx, k, Digest("true"), resolve, func(s LaunchSnapshot) (Binding, error) {
		return newTestBinding(t, j, k, s.Intent, nil)
	})
}

func TestDeniedContextJobReclaimsNoOutput(t *testing.T) {
	j, _, _, _, retained := terminalOutputFixture(t)
	j.maxActive = 1
	c := openTestContext(t, j, ownerA, "ctx", "/work")
	closed := openTestContext(t, j, ownerA, "closed", "/work")
	if _, err := j.CloseContext(t.Context(), ownerA, closed.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reserveJob(t, j, jobKey(ownerB, c.ID, "steal"), "x", nil); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("other caller's job = %v", err)
	}
	if _, _, err := reserveJob(t, j, jobKey(ownerA, closed.ID, "late"), "x", nil); !errors.Is(err, ErrContextClosed) {
		t.Fatalf("job in a closed context = %v", err)
	}
	after, err := j.Exact(t.Context(), retained.Binding)
	if err != nil || after.OutputExpired {
		t.Fatalf("a denied reservation reclaimed retained output: %+v %v", after.OutputExpired, err)
	}
	if _, err := j.read(jobKey(ownerB, c.ID, "steal").Hash()); err == nil {
		t.Fatal("denied reservation left a receipt")
	}
}

func TestContextJobsReplayAcrossEditsClosureAndCallers(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/work")
	d := openTestContext(t, j, ownerA, "ctx-2", "/work")
	first, owner, err := reserveJob(t, j, jobKey(ownerA, c.ID, "build"), "make", nil)
	if err != nil || !owner || first.Context.Revision != 1 {
		t.Fatalf("first reserve = %+v owner=%v err=%v", first.Context, owner, err)
	}
	second, owner, err := reserveJob(t, j, jobKey(ownerA, d.ID, "build"), "make", nil)
	if err != nil || !owner || second.Binding == first.Binding {
		t.Fatalf("distinct context reserve owner=%v err=%v", owner, err)
	}
	cwd := "/work/moved"
	if _, err := j.UpdateContext(t.Context(), ownerA, c.ID, 1, ContextChange{Cwd: &cwd}, func(ContextSpec) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := j.CloseContext(t.Context(), ownerA, c.ID, nil); err != nil {
		t.Fatal(err)
	}
	replay, owner, err := reserveJob(t, j, jobKey(ownerA, c.ID, "build"), "make", func(Context) { t.Error("replay resolved the context again") })
	if owner || replay.Binding != first.Binding || replay.Context.Revision != 1 {
		t.Fatalf("replay after edit and close = %+v owner=%v err=%v", replay.Context, owner, err)
	}
	if _, _, err := reserveJob(t, j, jobKey(ownerA, c.ID, "build"), "make clean", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed intent under one key = %v", err)
	}
	if _, _, err := reserveJob(t, j, jobKey(ownerA, c.ID, "new"), "make", nil); !errors.Is(err, ErrContextClosed) {
		t.Fatalf("new job in closed context = %v", err)
	}
	if _, _, err := reserveJob(t, j, jobKey(ownerB, d.ID, "x"), "make", nil); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("job in another caller's context = %v", err)
	}
	looked, err := j.Lookup(t.Context(), jobKey(ownerA, c.ID, "build"))
	if looked.Binding != first.Binding || !errors.Is(err, ErrProgress) {
		t.Fatalf("lookup after closure, before publication = %v", err)
	}
	if _, err := j.Lookup(t.Context(), jobKey(ownerB, c.ID, "build")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other caller lookup = %v", err)
	}
}

func TestContextJobPersistsItsLaunchSnapshot(t *testing.T) {
	j := contextJournal(t)
	c := openSpec(t, j, ownerA, "ctx", ContextSpec{Cwd: "/work", Shell: "sh", ShellPath: "/bin/sh", Env: map[string]string{"TOKEN": "one"}})
	r, owner, err := reserveJob(t, j, jobKey(ownerA, c.ID, "job"), "x", nil)
	if err != nil || !owner {
		t.Fatal(err)
	}
	cwd := "/work/other"
	if _, err := j.UpdateContext(t.Context(), ownerA, c.ID, 1, ContextChange{Cwd: &cwd, SetEnv: map[string]string{"TOKEN": "two"}}, func(ContextSpec) error { return nil }); err != nil {
		t.Fatal(err)
	}
	snapshot, err := j.ReadLaunchSnapshot(r)
	if err != nil || snapshot.Context.Revision != 1 || snapshot.Intent.Cwd != "/work" || snapshot.Env["TOKEN"] != "one" {
		t.Fatalf("snapshot after edit = %+v, %v", snapshot, err)
	}
	tampered := r
	tampered.LaunchIntentDigest = Digest("other")
	if _, err := j.ReadLaunchSnapshot(tampered); err == nil {
		t.Fatal("snapshot accepted for another receipt")
	}
}

func TestContextEditAndReservationAreOrdered(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/r/1")
	var group sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]uint64{}
	for i := range 8 {
		group.Go(func() {
			r, _, err := reserveJob(t, j, jobKey(ownerA, c.ID, fmt.Sprintf("job-%d", i)), "x", func(c Context) {
				if c.Cwd != fmt.Sprintf("/r/%d", c.Revision) {
					t.Errorf("reservation saw cwd %s at revision %d", c.Cwd, c.Revision)
				}
			})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen[r.Key.Opaque] = r.Context.Revision
			mu.Unlock()
		})
		group.Go(func() { updateUntilApplied(t, j, c.ID) })
	}
	group.Wait()
	final, err := j.GetContext(t.Context(), ownerA, c.ID)
	if err != nil || final.Revision != 9 || len(seen) != 8 || len(final.Members) != 8 {
		t.Fatalf("final revision %d, %d receipts, %d members, %v", final.Revision, len(seen), len(final.Members), err)
	}
}

// updateUntilApplied retries on conflict; each applied edit moves cwd to
// /r/<new revision>, so a reservation can check it saw one consistent state.
func updateUntilApplied(t *testing.T, j *Journal, id string) {
	for {
		c, err := j.GetContext(t.Context(), ownerA, id)
		if err != nil {
			t.Error(err)
			return
		}
		cwd := fmt.Sprintf("/r/%d", c.Revision+1)
		_, err = j.UpdateContext(t.Context(), ownerA, id, c.Revision, ContextChange{Cwd: &cwd}, func(ContextSpec) error { return nil })
		if !errors.Is(err, ErrContextConflict) {
			if err != nil {
				t.Error(err)
			}
			return
		}
	}
}

func TestContextExpiryWaitsForActiveJobsEvenAfterFailedWrites(t *testing.T) {
	j := contextJournal(t)
	idle := openTestContext(t, j, ownerA, "idle", "/work")
	busy := openTestContext(t, j, ownerA, "busy", "/work")
	if _, _, err := reserveJob(t, j, jobKey(ownerA, busy.ID, "run"), "x", nil); err != nil {
		t.Fatal(err)
	}
	// Losing the context's own last-use write must not unprotect the job.
	record, err := j.readContext(busy.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.LastUsedAt = record.CreatedAt
	if _, err := j.saveContext(record); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(31 * 24 * time.Hour)
	j.now = func() time.Time { return later }
	if _, err := j.GetContext(t.Context(), ownerA, idle.ID); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("idle context after 31 days = %v", err)
	}
	if found, _, _ := j.readRequest(openKey(ownerA, "idle").Hash()); found.ContextID != "" {
		t.Fatal("expired open receipt kept")
	}
	kept, err := j.GetContext(t.Context(), ownerA, busy.ID)
	if err != nil || len(kept.Members) != 1 {
		t.Fatalf("busy context = %+v, %v", kept, err)
	}
	if err := j.MaintainContexts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := j.GetContext(t.Context(), ownerA, busy.ID); err != nil {
		t.Fatalf("maintenance expired a context with an active job: %v", err)
	}
	fresh := openTestContext(t, j, ownerA, "idle", "/work")
	if fresh.ID == idle.ID {
		t.Fatal("expired context came back")
	}
}

func TestContextSurvivesReboot(t *testing.T) {
	root := t.TempDir()
	j, err := Open(root, "boot-a", "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	c := openTestContext(t, j, ownerA, "ctx", "/work")
	if _, _, err := reserveJob(t, j, jobKey(ownerA, c.ID, "run"), "x", nil); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	rebooted, err := Open(root, "boot-b", "coordinator-2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebooted.Close() }()
	after, err := rebooted.GetContext(t.Context(), ownerA, c.ID)
	if err != nil || after.ClosedAt != nil || after.ID != c.ID {
		t.Fatalf("context after reboot = %+v, %v", after, err)
	}
}

// TestContextRecordHoldsWorstCaseEncoding fills every bounded field with a
// byte JSON escapes to six.
func TestContextRecordHoldsWorstCaseEncoding(t *testing.T) {
	j := contextJournal(t)
	env := map[string]string{}
	for i := 0; i < 128; i++ {
		env[fmt.Sprintf("V%03d", i)] = strings.Repeat("<", (32<<10)/128-6)
	}
	spec := ContextSpec{Cwd: "/" + strings.Repeat("<", 4095), Shell: "login", ShellPath: "/" + strings.Repeat("<", 4095), Env: env, Label: strings.Repeat("<", 256)}
	c := openSpec(t, j, ownerA, "big", spec)
	got, err := j.GetContext(t.Context(), ownerA, c.ID)
	if err != nil || got.Env["V000"] != env["V000"] {
		t.Fatalf("worst-case record = %v", err)
	}
	if _, err := j.UpdateContext(t.Context(), ownerA, c.ID, 1, ContextChange{}, func(ContextSpec) error { return nil }); err != nil {
		t.Fatalf("worst-case update = %v", err)
	}
	if _, _, err := reserveJob(t, j, jobKey(ownerA, c.ID, "job"), "x", nil); err != nil {
		t.Fatalf("worst-case launch snapshot = %v", err)
	}
}

func TestContextListPages(t *testing.T) {
	j := contextJournal(t)
	for i := range 5 {
		openTestContext(t, j, ownerA, fmt.Sprintf("c-%d", i), "/work")
	}
	openTestContext(t, j, ownerB, "other", "/work")
	seen := map[string]bool{}
	cursor := ""
	for range 10 {
		page, more, err := j.ListContexts(t.Context(), ownerA, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			if seen[c.ID] || c.ID <= cursor {
				t.Fatalf("page repeated or reordered %s", c.ID)
			}
			seen[c.ID] = true
			cursor = c.ID
		}
		if !more {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pages returned %d of 5 contexts", len(seen))
	}
}
