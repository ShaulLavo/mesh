package cli

import (
	"context"
	"fmt"
	"syscall"
	"testing"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/worker"
)

func TestProbeLivenessClassifiesWorkerSocketErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want Liveness
	}{
		{"answering", nil, LivenessAlive},
		{"missing", fmt.Errorf("dial: %w", syscall.ENOENT), LivenessGone},
		{"refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), LivenessGone},
		{"timeout", context.DeadlineExceeded, LivenessUnknown},
		{"backlog full", syscall.EAGAIN, LivenessUnknown},
		{"permission denied", syscall.EACCES, LivenessUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupCommandTestHost(t)
			writeLocalSessionDir(t, "PR0B", worker.StateRunning)
			setWorkerProbe(t, func(string) error { return test.err })
			current, err := Find("PR0B")
			if err != nil || current.Liveness != test.want {
				t.Fatalf("liveness = %v, %v, want %v", current.Liveness, err, test.want)
			}
		})
	}
}

func TestListReportsRebootedSessionAsInterruptedWithoutProbing(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	dir, err := paths.SessionDir("PR0B")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := worker.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	meta.BootID = "previous-boot"
	if worker.BootID() == "" {
		t.Fatal("host did not provide its boot ID")
	}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	setWorkerProbe(t, func(string) error {
		t.Fatal("rebooted session probed a socket from the wrong boot")
		return nil
	})
	current, err := Find("PR0B")
	if err != nil || current.Liveness != LivenessGone || current.State() != worker.StateInterrupted {
		t.Fatalf("rebooted session = %+v, %v", current, err)
	}
}

func TestSessionStatePreservesUnknownAndDistinguishesGone(t *testing.T) {
	for _, state := range []string{worker.StateRunning, worker.StateDetached} {
		current := Session{Meta: worker.Meta{State: state}, Liveness: LivenessUnknown}
		if got := current.State(); got != state {
			t.Errorf("unknown %s = %s", state, got)
		}
		current.Liveness = LivenessGone
		if got := current.State(); got != worker.StateInterrupted {
			t.Errorf("gone %s = %s", state, got)
		}
	}
}
