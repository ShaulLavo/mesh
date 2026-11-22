package execution

import (
	"errors"
	"fmt"
	"os"
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

func openTestContext(t *testing.T, j *Journal, owner, key, cwd string) Context {
	t.Helper()
	c, created, err := j.OpenContext(t.Context(), openKey(owner, key), ContextSpec{Cwd: cwd, Shell: "sh", ShellPath: "/bin/sh"})
	if err != nil || !created {
		t.Fatalf("open %s: created=%v err=%v", key, created, err)
	}
	return c
}

// reserveJob reserves a pipe job whose argv is fixed and whose cwd comes
// from the context revision it sees.
func reserveJob(t *testing.T, j *Journal, k Key, request string, seen func(Context)) (Receipt, bool, error) {
	t.Helper()
	resolve := func(c Context) (Intent, error) {
		if seen != nil {
			seen(c)
		}
		return Intent{Kind: KindPipe, Command: []string{"/bin/echo", request}, Cwd: c.Cwd, Stdin: StdinClosed}, nil
	}
	return j.ReserveInContext(t.Context(), k, Digest(request), resolve, func(i Intent) (Binding, error) {
		return newTestBinding(t, j, k, i, []string{"PATH=/bin"})
	})
}

func TestContextOpenReplayConflictAndLostReply(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/work")
	again, created, err := j.OpenContext(t.Context(), openKey(ownerA, "ctx-1"), ContextSpec{Cwd: "/work", Shell: "sh", ShellPath: "/bin/sh"})
	if err != nil || created || again.ID != c.ID {
		t.Fatalf("replay = %s created=%v err=%v", again.ID, created, err)
	}
	if _, _, err := j.OpenContext(t.Context(), openKey(ownerA, "ctx-1"), ContextSpec{Cwd: "/other", Shell: "sh", ShellPath: "/bin/sh"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed intent = %v", err)
	}
	// A lost reply is recovered by key, even after closure.
	if _, err := j.CloseContext(t.Context(), ownerA, c.ID, nil); err != nil {
		t.Fatal(err)
	}
	found, err := j.ContextReceipt(t.Context(), openKey(ownerA, "ctx-1"))
	if err != nil || found.ID != c.ID || found.ClosedAt == nil {
		t.Fatalf("receipt after close = %+v, %v", found, err)
	}
	if _, err := j.ContextReceipt(t.Context(), openKey(ownerB, "ctx-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other caller's receipt lookup = %v", err)
	}
	// The same key in another caller's namespace opens a separate context.
	other := openTestContext(t, j, ownerB, "ctx-1", "/work")
	if other.ID == c.ID {
		t.Fatal("two callers share one context")
	}
}

func TestContextRecordLostAfterReceiptIsCompleted(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/work")
	if err := j.files.Remove(contextName(c.ID), "execution/.barrier"); err != nil {
		t.Fatal(err)
	}
	found, err := j.ContextReceipt(t.Context(), openKey(ownerA, "ctx-1"))
	if err != nil || found.ID != c.ID || found.Revision != 1 {
		t.Fatalf("receipt completion = %+v, %v", found, err)
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
	if list, err := j.ListContexts(t.Context(), ownerB); err != nil || len(list) != 0 {
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
}

func TestContextJobsReplayAcrossEditsClosureAndCallers(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx-1", "/work")
	d := openTestContext(t, j, ownerA, "ctx-2", "/work")
	first, owner, err := reserveJob(t, j, jobKey(ownerA, c.ID, "build"), "make", nil)
	if err != nil || !owner || first.Context.Revision != 1 {
		t.Fatalf("first reserve = %+v owner=%v err=%v", first.Context, owner, err)
	}
	// Equal input in a distinct context is a distinct job.
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
	if err != nil || final.Revision != 9 || len(seen) != 8 {
		t.Fatalf("final revision %d, %d receipts, %v", final.Revision, len(seen), err)
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

func TestContextExpiryWaitsForRunningJobs(t *testing.T) {
	j := contextJournal(t)
	idle := openTestContext(t, j, ownerA, "idle", "/work")
	busy := openTestContext(t, j, ownerA, "busy", "/work")
	if _, _, err := reserveJob(t, j, jobKey(ownerA, busy.ID, "run"), "x", nil); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(31 * 24 * time.Hour)
	j.now = func() time.Time { return later }
	if _, err := j.GetContext(t.Context(), ownerA, idle.ID); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("idle context after 31 days = %v", err)
	}
	if _, err := os.Stat(j.files.root.Name() + "/" + contextRequestName(openKey(ownerA, "idle").Hash())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired open receipt kept: %v", err)
	}
	kept, err := j.GetContext(t.Context(), ownerA, busy.ID)
	if err != nil || kept.RunningJobs != 1 {
		t.Fatalf("busy context = %+v, %v", kept, err)
	}
	// After expiry, the old open key starts a fresh context.
	fresh := openTestContext(t, j, ownerA, "idle", "/work")
	if fresh.ID == idle.ID {
		t.Fatal("expired context came back")
	}
	if err := j.MaintainContexts(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestContextSurvivesRebootWithoutRunningJobs(t *testing.T) {
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
	if err != nil || after.RunningJobs != 0 || after.ClosedAt != nil {
		t.Fatalf("context after reboot = %+v, %v", after, err)
	}
}

func TestContextLimitPerOwner(t *testing.T) {
	j := contextJournal(t)
	for i := range maxOpenPerOwner {
		openTestContext(t, j, ownerA, fmt.Sprintf("c-%d", i), "/work")
	}
	if _, _, err := j.OpenContext(t.Context(), openKey(ownerA, "one-more"), ContextSpec{Cwd: "/work", Shell: "none"}); !errors.Is(err, ErrContextLimit) {
		t.Fatalf("65th context = %v", err)
	}
	openTestContext(t, j, ownerB, "c-0", "/work")
}
