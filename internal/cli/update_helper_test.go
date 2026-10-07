package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHelperUpgradeReusesPersistedDaemonServiceDomain(t *testing.T) {
	settings := updateinstall.Settings{Service: updateinstall.ServiceSpec{Kind: "launchd", Domain: "user/501"}}
	kind, domain := helperUpgradeService(settings)
	if kind != "launchd" || domain != "user/501" {
		t.Fatalf("helper upgrade service = %s %s, want persisted launchd user domain", kind, domain)
	}
}

func TestUpdateHelperWaitsAfterSlowDiagnostic(t *testing.T) {
	state := t.TempDir()
	journal := filepath.Join(state, "update", "installation.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("!"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	writer := &helperSlowDiagnostic{journal: journal, events: make(chan helperDiagnosticEvent, 2)}
	done := make(chan error, 1)
	go func() { done <- runUpdateHelper(ctx, state, writer) }()
	first := receiveHelperDiagnostic(ctx, t, writer.events)
	second := receiveHelperDiagnostic(ctx, t, writer.events)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if elapsed := second.at.Sub(first.at); elapsed < time.Second {
		t.Fatalf("next helper check started %s after slow work, want a fresh one-second wait", elapsed)
	}
}

type helperDiagnosticEvent struct {
	at  time.Time
	err error
}

type helperSlowDiagnostic struct {
	journal string
	events  chan helperDiagnosticEvent
	calls   int
}

func (w *helperSlowDiagnostic) Write(data []byte) (int, error) {
	w.calls++
	if w.calls != 1 {
		w.events <- helperDiagnosticEvent{at: time.Now()}
		return len(data), nil
	}
	time.Sleep(1100 * time.Millisecond)
	err := os.WriteFile(w.journal, []byte("?"), 0600)
	w.events <- helperDiagnosticEvent{at: time.Now(), err: err}
	return len(data), err
}

func receiveHelperDiagnostic(ctx context.Context, t *testing.T, events <-chan helperDiagnosticEvent) helperDiagnosticEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.err != nil {
			t.Fatal(event.err)
		}
		return event
	case <-ctx.Done():
		t.Fatal("helper did not report the next distinct fixture problem")
		return helperDiagnosticEvent{}
	}
}

func TestUpdateHelperCancellationInterruptsIdleWait(t *testing.T) {
	state := t.TempDir()
	journal := filepath.Join(state, "update", "installation.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("!"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{}, 1)
	writer := helperDiagnosticWriter(func(data []byte) (int, error) {
		ready <- struct{}{}
		return len(data), nil
	})
	done := make(chan error, 1)
	go func() { done <- runUpdateHelper(ctx, state, writer) }()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("helper did not complete its first check")
	}
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("helper cancellation waited for the next poll")
	}
}

type helperDiagnosticWriter func([]byte) (int, error)

func (write helperDiagnosticWriter) Write(data []byte) (int, error) { return write(data) }
