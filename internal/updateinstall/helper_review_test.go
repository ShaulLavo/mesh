package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"golang.org/x/sys/unix"
)

func TestReviewInterruptedHelperPromotion(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	candidate, err := prepareHelper(t.Context(), f.cfg, true, "")
	if err != nil {
		t.Fatal(err)
	}

	old, _ := json.Marshal(f.prior)
	if err = atomicWrite(helperRecord(f.cfg.StateDir), old, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := UpgradeHelper(t.Context(), f.cfg)
	if err != nil || got != candidate {
		t.Fatalf("committed promotion cannot resume: %v", err)
	}
}

func TestReviewApprovedCandidateSwap(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.150", "")
	older, err := os.ReadFile(f.cfg.Executable) //nolint:gosec // disposable installation fixture
	if err != nil {
		t.Fatal(err)
	}
	swap := filepath.Join(t.TempDir(), "replacement")
	if err = atomicWrite(swap, older, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_REVIEW_SWAP", swap)
	f.commit(t, "v0.1.171", "")
	data, err := os.ReadFile(f.cfg.Executable) //nolint:gosec // disposable installation fixture
	if err != nil {
		t.Fatal(err)
	}

	data = []byte(strings.Replace(string(data), "  exit 0\nfi", "  mv \"$MESH_REVIEW_SWAP\" \"$0\"\n  exit 0\nfi", 1))
	if err = atomicWrite(f.cfg.Executable, data, 0755); err != nil {
		t.Fatal(err)
	}
	approved := digestBytes(data)
	f.publish(t, release.Build{Version: "v0.1.171", Commit: strings.Repeat("a", 40), Digest: approved, StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1})
	before := f.snapshot(t)
	before[f.cfg.Executable] = string(older)
	if _, err = UpgradeHelper(t.Context(), f.cfg); err == nil {
		t.Fatal("source replacement after approval was accepted")
	}
	f.assertUnchanged(t, before)
}

func TestReviewMetadataInheritedPipeCancellation(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "metadata")
	if err := atomicWrite(exe, []byte("#!/bin/sh\nsleep 1 &\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := helperBuild(ctx, exe, strings.Repeat("a", 64))
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("cancelled metadata probe drained inherited pipes for %s (error_present=%t)", elapsed, err != nil)
	}
}

func TestReviewMetadataAllocationBound(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "metadata")
	if err := atomicWrite(exe, []byte("#!/bin/sh\nwhile :; do printf '%8192s' x; printf '%8192s' x >&2; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := helperBuild(ctx, exe, strings.Repeat("a", 64))
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("streaming stdout/stderr overflow allocated %d bytes and stopped in %s", allocated, time.Since(started))
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("overflow did not stop the streaming producer promptly")
	}
	if err == nil {
		t.Fatal("oversized metadata accepted")
	}
	if allocated > 1<<20 {
		t.Fatalf("64 KiB accepted-report limit retained oversized output: Go allocated %d bytes for 4 MiB fixture", allocated)
	}
}

func TestReviewReceiptVerificationCancellation(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	if err := os.Remove(f.prior.Executable); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(f.prior.Executable, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := UpgradeHelper(ctx, f.cfg); done <- err }()
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):

		writer, err := os.OpenFile(f.prior.Executable, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte("fixture"))
		_ = writer.Close()
		<-done
		t.Fatal("caller cancellation did not bound receipt executable verification")
	}
}

func TestUpgradeHelperPromotionBoundaries(t *testing.T) {
	for _, boundary := range []string{"copy", "service", "link", "receipt"} {
		for _, kind := range []string{"systemd", "launchd"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				f := newHelperUpgradeFixture(t, kind, "v0.1.170")
				f.commit(t, "v0.1.171", "")
				before := f.snapshot(t)
				installed, err := prepareHelper(t.Context(), f.cfg, true, "")
				if err != nil {
					t.Fatal(err)
				}
				if boundary != "receipt" {
					if err = atomicWrite(helperRecord(f.cfg.StateDir), []byte(before[helperRecord(f.cfg.StateDir)]), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "copy" || boundary == "service" {
					if err = replaceHelperLink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"), f.prior.Executable); err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "copy" {
					if err = atomicWrite(f.prior.ServicePath, []byte(before[f.prior.ServicePath]), 0644); err != nil {
						t.Fatal(err)
					}
				}
				got, err := UpgradeHelper(t.Context(), f.cfg)
				if err != nil || got != installed {
					t.Fatalf("interrupted promotion at %s failed: %v", boundary, err)
				}
				var receipt HelperInstallation
				if err = readJSON(helperRecord(f.cfg.StateDir), &receipt); err != nil || receipt != installed {
					t.Fatalf("recovered receipt differs: %v", err)
				}
				link, err := os.Readlink(filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current"))
				if err != nil || link != installed.Executable {
					t.Fatalf("recovered launcher differs: %v", err)
				}
				if err = release.VerifyExecutable(t.Context(), f.prior.Executable, f.prior.Digest); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{f.cfg.Executable, journalPath(f.cfg.StateDir)} {
					data, err := os.ReadFile(path) //nolint:gosec // disposable installation fixture
					if err != nil || string(data) != before[path] {
						t.Fatalf("promotion changed daemon/journal: %v", err)
					}
				}
			})
		}
	}
}

func TestUpgradeHelperSourceVerificationCancellation(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	if err := os.Remove(f.cfg.Executable); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(f.cfg.Executable, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := UpgradeHelper(ctx, f.cfg); err == nil || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("source special file ignored verification bound: %v", err)
	}
}

func TestUpgradeHelperCancelledVerification(t *testing.T) {
	f := newHelperUpgradeFixture(t, "systemd", "v0.1.170")
	f.commit(t, "v0.1.171", "")
	before := f.snapshot(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := UpgradeHelper(ctx, f.cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled source verification = %v", err)
	}
	f.assertUnchanged(t, before)
}
