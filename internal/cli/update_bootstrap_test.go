package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestBootstrapCommandRemainsUntilExactInstallationFinishes(t *testing.T) {
	stateDir, local := setupUpdateCLI(t)
	status := updateinstall.Status{
		Schema: 1,
		Phase:  updateinstall.Granted,
		Settings: updateinstall.Settings{
			StateDir: stateDir,
		},
		Request: updateinstall.Request{
			ID:         strings.Repeat("a", 32),
			TargetID:   local.ID,
			Generation: 1,
			Manifest:   updateTestManifest(),
		},
	}
	writeApprovalJournal(t, status)
	type result struct {
		status updateinstall.Status
		err    error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func(initial updateinstall.Status) {
		finished, err := waitBootstrapInstallation(ctx, stateDir, initial)
		done <- result{status: finished, err: err}
	}(status)
	select {
	case result := <-done:
		t.Fatalf("bootstrap worker exited while activation remained granted: %+v", result)
	case <-time.After(150 * time.Millisecond):
	}
	status.Phase = updateinstall.Committed
	writeApprovalJournal(t, status)
	select {
	case result := <-done:
		if result.err != nil || result.status.Phase != updateinstall.Committed {
			t.Fatalf("bootstrap terminal receipt = %s, %v", result.status.Phase, result.err)
		}
	case <-ctx.Done():
		t.Fatal("bootstrap worker did not exit after terminal receipt")
	}
}

func TestBootstrapWaitRejectsReplacementInstallation(t *testing.T) {
	stateDir, local := setupUpdateCLI(t)
	status := updateinstall.Status{Schema: 1, Phase: updateinstall.Granted, Settings: updateinstall.Settings{StateDir: stateDir}, Request: updateinstall.Request{
		ID: strings.Repeat("a", 32), TargetID: local.ID, Generation: 1, Manifest: updateTestManifest(),
	}}
	replacement := status
	replacement.Request.ID = strings.Repeat("b", 32)
	writeApprovalJournal(t, replacement)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := waitBootstrapInstallation(ctx, stateDir, status); err == nil || !strings.Contains(err.Error(), "installation changed") {
		t.Fatalf("replacement installation ended bootstrap wait: %v", err)
	}
}
