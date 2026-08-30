package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/dnsname"
)

func TestPrivateNamesReconcileRequiresExplicitEnvironmentAndTerms(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	var requests []PrivateNamesRequest
	dependencies := Dependencies{ReconcilePrivateNames: func(_ context.Context, request PrivateNamesRequest) error {
		requests = append(requests, request)
		return nil
	}}
	stdout, _, err := executeCommand(t, dependencies,
		"private-names", "reconcile", "--config", "/etc/mesh/private-names.json", "--staging", "--force", "--accept-tos",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
	request := requests[0]
	if request.ConfigPath != "/etc/mesh/private-names.json" || request.DirectoryURL != dnsname.LetsEncryptStagingURL || !request.AcceptTerms || !request.Force || request.StateDir == "" {
		t.Fatalf("request = %#v", request)
	}
	if !strings.Contains(stdout, "(staging)") {
		t.Fatalf("stdout = %q", stdout)
	}

	requests = nil
	stdout, _, err = executeCommand(t, dependencies,
		"private-names", "reconcile", "--config", "/etc/mesh/private-names.json", "--live", "--accept-tos",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].DirectoryURL != dnsname.LetsEncryptProductionURL || requests[0].Force {
		t.Fatalf("live requests = %#v", requests)
	}
	if !strings.Contains(stdout, "(live)") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestPrivateNamesReconcileRejectsIncompleteBoundaryBeforeRunning(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	calls := 0
	dependencies := Dependencies{ReconcilePrivateNames: func(context.Context, PrivateNamesRequest) error {
		calls++
		return nil
	}}
	for name, arguments := range map[string][]string{
		"missing config":      {"private-names", "reconcile", "--staging", "--accept-tos"},
		"missing environment": {"private-names", "reconcile", "--config", "/config", "--accept-tos"},
		"both environments":   {"private-names", "reconcile", "--config", "/config", "--staging", "--live", "--accept-tos"},
		"missing terms":       {"private-names", "reconcile", "--config", "/config", "--live"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := executeCommand(t, dependencies, arguments...); err == nil {
				t.Fatal("invalid reconcile command succeeded")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid commands ran reconciliation %d times", calls)
	}
}

func TestDaemonExposesPrivateHTTPSOperationalFlags(t *testing.T) {
	command := NewCommand(Dependencies{})
	daemonCommand, _, err := command.Find([]string{"daemon"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"https-port", "certificate-renewer-id", "private-names-config", "tailscale-serve", "tailnet-port", "websocket-path"} {
		if daemonCommand.Flags().Lookup(name) == nil {
			t.Fatalf("daemon flag --%s is missing", name)
		}
	}
	if _, _, err := executeCommand(t, Dependencies{}, "daemon", "--https-port", "65536"); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("HTTPS port boundary error = %v", err)
	}
}
