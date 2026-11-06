//go:build linux

package apps

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestDescriptorObservationErrorsDiscardPartialEvidence(t *testing.T) {
	for _, operation := range []string{"directory", "readlink"} {
		t.Run(operation, func(t *testing.T) { testDescriptorErrors(t, operation) })
	}
}

func testDescriptorErrors(t *testing.T, operation string) {
	t.Helper()
	for _, cause := range []error{syscall.EACCES, syscall.EPERM, syscall.EIO} {
		t.Run(cause.Error(), func(t *testing.T) { testDescriptorObservationError(t, operation, cause) })
	}
}

func testDescriptorObservationError(t *testing.T, operation string, cause error) {
	t.Helper()
	names := func(path string) ([]string, error) {
		if operation == "directory" && path == "/proc/12/fd" {
			return nil, &os.PathError{Op: "open", Path: path, Err: cause}
		}
		return []string{"3"}, nil
	}
	readlink := func(path string) (string, error) {
		if operation == "readlink" && path == "/proc/12/fd/3" {
			return "", &os.PathError{Op: "readlink", Path: path, Err: cause}
		}
		return "socket:[41]", nil
	}
	held, err := heldSocketsFrom([]int{11, 12}, names, readlink)
	if held != nil || !errors.Is(err, cause) || !strings.Contains(err.Error(), "/proc/12/fd") {
		t.Fatalf("partial evidence = %v, %v; want discarded map and descriptor diagnostic", held, err)
	}
	if incomplete := errors.Is(err, errIncompleteListenerObservation); incomplete != errors.Is(cause, os.ErrPermission) {
		t.Fatalf("incomplete classification for %v = %t", cause, incomplete)
	}
}

func TestDescriptorObservationDisappearanceAndCompleteControl(t *testing.T) {
	names := func(path string) ([]string, error) {
		if path == "/proc/12/fd" {
			return nil, fmt.Errorf("gone: %w", os.ErrNotExist)
		}
		return []string{"3", "4", "5"}, nil
	}
	readlink := func(path string) (string, error) {
		if strings.HasSuffix(path, "/4") {
			return "", fmt.Errorf("closed: %w", os.ErrNotExist)
		}
		if strings.HasSuffix(path, "/5") {
			return "/dev/null", nil
		}
		return "socket:[41]", nil
	}
	held, err := heldSocketsFrom([]int{11, 12}, names, readlink)
	if err != nil || len(held) != 1 || !held[41] {
		t.Fatalf("complete evidence with disappeared process and descriptor = %v, %v", held, err)
	}
}

type incompleteProcessWorkers struct {
	*fakeWorkers
	calls int
}

func (w *incompleteProcessWorkers) Processes(context.Context, string) ([]int, error) {
	w.calls++
	return nil, errIncompleteListenerObservation
}

func TestPublishedAppSuspendsOnIncompleteObservation(t *testing.T) {
	f := newAppFixture(t)
	workers := &incompleteProcessWorkers{fakeWorkers: f.workers}
	f.origin.config.Workers = workers
	f.workers.labels = map[string]string{"app probe": "session"}
	f.origin.state.Apps["probe"] = localApp{
		Record: Record{ID: "probe", Kind: "server", Status: "active"},
		Phase:  "ready", Command: "serve", Port: 3000,
	}
	f.origin.setServing("probe", serving{upstream: netip.MustParseAddrPort("127.0.0.1:3000"), inode: 41})
	err := f.origin.checkServing(context.Background(), "probe")
	if !errors.Is(err, errIncompleteListenerObservation) || workers.calls != 1 {
		t.Fatalf("published observation = %v, calls %d; want immediate refusal", err, workers.calls)
	}
	if route := (*f.origin.routes.Load())["probe"]; route.Handler != nil || route.Upstream.IsValid() {
		t.Fatalf("incomplete published observation retained route: %+v", route)
	}
}
