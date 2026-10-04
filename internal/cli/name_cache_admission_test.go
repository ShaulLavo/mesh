package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAuthenticatedNameCacheLockStillGatesEffects(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), "machine-names", f.host.ID+".lock"), os.O_RDWR, 0) //nolint:gosec // fixture cache lock
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // fixture descriptor cleanup releases lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	for _, argument := range []string{f.host.ID, "destination"} {
		target, err := ResolveArgument(argument, hosts)
		if err != nil || target.Host == nil {
			t.Fatalf("target %s: %+v %v", argument, target, err)
		}
		start := time.Now()
		_, err = listRemoteHost(t.Context(), *target.Host, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout})
		t.Logf("target=%s held-lock elapsed=%s error=%v", argument, time.Since(start), err)
		if !errors.Is(err, context.DeadlineExceeded) || f.operations.Load() != 0 {
			t.Fatalf("cache admission bypassed before effects: error=%v operations=%d", err, f.operations.Load())
		}
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := listRemoteHost(t.Context(), f.host, dialControlHost, HostQueryBudget{Setup: remoteConnectTimeout, Reply: defaultCatalogTimeout}); err != nil || f.operations.Load() != 1 {
		t.Fatalf("released admission did not recover: error=%v operations=%d", err, f.operations.Load())
	}
}
