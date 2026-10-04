package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/tunnel"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestBootstrapUnprovenOwnerCannotSaveClaim(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing pin", true: "foreign owner"}[foreign], func(t *testing.T) {
			f := namedDestination(t)
			f.host.MachineName, f.host.NameRevision = "forged", 99
			pin := ""
			if foreign {
				owner, _, err := identity.LoadOrCreate(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				pin = owner.ID
			}
			_, _, err := executeCommand(t, Dependencies{Bootstrap: func(context.Context, AddRequest) (BootstrapResult, error) {
				return BootstrapResult{Host: f.host, AuthenticatedIdentity: pin}, nil
			}}, "add", "fixture.test")
			if err == nil {
				t.Fatal("unproven bootstrap result saved a foreign claim")
			}
			config, loadErr := loadHostConfig()
			if loadErr != nil || len(config.Hosts) != 0 {
				t.Fatalf("refused bootstrap saved host: %+v %v", config, loadErr)
			}
			path, _ := ConfigPath()
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "machine-names", f.host.ID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused bootstrap cached foreign claim: %v", err)
			}
		})
	}
}

func TestAppNameVerificationPrecedesSSH(t *testing.T) {
	for _, mode := range []string{"fresh", "renamed", "conflict", "exact ID after rename"} {
		t.Run(mode, func(t *testing.T) {
			f := namedDestination(t)
			f.host.MachineName, f.host.NameRevision = "destination", 1
			if err := saveNamedTestHost(t, f.host); err != nil {
				t.Fatal(err)
			}
			if mode == "renamed" || mode == "exact ID after rename" {
				if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "conflict" {
				other := namedDestinationPeer(t)
				other.host.MachineName, other.host.NameRevision = "destination", 1
				if err := saveNamedTestHost(t, other.host); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			var attempts atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err == nil {
					attempts.Add(1)
					_ = conn.Close()
				}
			}()
			a := application{dependencies: Dependencies{DialControl: dialControlHost}}
			target := "destination"
			if mode == "exact ID after rename" {
				target = f.host.ID
			}
			port := listener.Addr().(*net.TCPAddr).Port
			if port < 1 || port > 65535 {
				t.Fatalf("invalid assigned fixture port %d", port)
			}
			opened, err := a.openApps(t.Context(), target, uint16(port)) //nolint:gosec // net.Listen assigns a valid TCP port; its bounds are checked above
			if err == nil {
				_ = opened.close()
				t.Fatal("fixture SSH unexpectedly succeeded")
			}
			_ = listener.Close()
			<-done
			if mode == "fresh" || mode == "exact ID after rename" {
				if attempts.Load() != 1 {
					t.Fatalf("fresh name never reached SSH: %v", err)
				}
				return
			}
			if attempts.Load() != 0 {
				t.Fatalf("%s name reached SSH before refusal: %v", mode, err)
			}
		})
	}
}

func TestPublishedCoordinatorRefusalPrecedesPlan(t *testing.T) {
	for _, current := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "current"}[current], func(t *testing.T) {
			_, local := setupUpdateCLI(t)
			remoteIdentity, _, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			remote := HostRecord{ID: remoteIdentity.ID, MeshIdentity: remoteIdentity.ID, Endpoint: "ws://coordinator.invalid/mesh", MachineName: "coordinator", NameRevision: 1}
			if err := saveNamedTestHost(t, remote); err != nil {
				t.Fatal(err)
			}
			client, _ := updateTestRelease(t)
			var plans atomic.Int32
			caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
				if action == "info" {
					build := updateTestBuild()
					build.Version = "v0.1.167"
					data, _ := json.Marshal(map[string]any{"health": updateinstall.Health{HostID: host.ID, Build: build}, "acceptsIdentityFleet": current})
					return json.Unmarshal(data, output)
				}
				if action != "plan" {
					return errors.New("unexpected update effect")
				}
				plans.Add(1)
				if !current {
					return errors.New("update: invalid host alias")
				}
				plan := input.(update.Plan)
				*output.(*update.Run) = update.Run{ID: strings.Repeat("a", 32), Fleet: plan.Fleet, Release: plan.Manifest, Targets: []update.Target{{Host: local, State: update.Updated}}}
				return nil
			})
			_, _, err = executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--coordinator", remote.ID, "--yes", "--json")
			if current {
				if err != nil || plans.Load() != 1 {
					t.Fatalf("current coordinator rejected: %v plans=%d", err, plans.Load())
				}
				return
			}
			if err == nil || plans.Load() != 0 || !strings.Contains(err.Error(), "v0.1.167") || !strings.Contains(err.Error(), "older Mesh") || !strings.Contains(err.Error(), "mesh update --local on that machine first") {
				t.Fatalf("published coordinator guidance/effect: err=%v plans=%d", err, plans.Load())
			}
		})
	}
}

func TestTunnelStaleNameCannotPersistSignedAttempt(t *testing.T) {
	f := namedDestination(t)
	f.host.MachineName, f.host.NameRevision = "destination", 1
	if err := saveNamedTestHost(t, f.host); err != nil {
		t.Fatal(err)
	}
	host, err := resolveHostTarget([]HostRecord{f.host}, "destination")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "renamed", 1); err != nil {
		t.Fatal(err)
	}
	a := application{dependencies: Dependencies{DialControl: dialControlHost}}
	_, err = a.deliverTunnelMutation(t.Context(), host, tunnel.Create, "fixture.example.test")
	if err == nil {
		t.Fatal("stale tunnel name reached signed effect")
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, catalogDatabaseName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale name created durable tunnel outbox/catalog: %v", err)
	}
}

func TestHostConfigUnknownFieldsAreRefused(t *testing.T) {
	for _, field := range []string{"alias", "unknown", "root", "dashboard"} {
		t.Run(field, func(t *testing.T) {
			f := namedDestination(t)
			data, _ := json.Marshal(f.host)
			var host map[string]any
			_ = json.Unmarshal(data, &host)
			config := map[string]any{"version": 1, "hosts": []any{host}}
			switch field {
			case "root":
				config["unknown"] = "typo"
			case "dashboard":
				config["dashboard"] = map[string]any{"unknown": "typo"}
			default:
				host[field] = "typo"
			}
			data, _ = json.Marshal(config)
			path, _ := ConfigPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadHosts(); err == nil {
				t.Fatalf("unknown %s accepted", field)
			}
		})
	}
}

func TestLocalClientUpgradeRefusesPublishedDaemonBeforePlan(t *testing.T) {
	_, local := setupUpdateCLI(t)
	client, requests := updateTestRelease(t)
	var plans atomic.Int32
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if action != "info" {
			plans.Add(1)
			return errors.New("unexpected plan effect")
		}
		build := updateTestBuild()
		build.Version = "v0.1.167"
		*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: local.ID, Build: build}}
		return nil
	})
	_, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--local", "--yes", "--json")
	if err == nil || plans.Load() != 0 || requests.Load() != 0 || !strings.Contains(err.Error(), "older Mesh v0.1.167") || !strings.Contains(err.Error(), "mesh daemon install") {
		t.Fatalf("client-only upgrade reached old plan or omitted restart guidance: %v effects=%d releases=%d", err, plans.Load(), requests.Load())
	}
}

func TestDuplicateOwnerInputsRefuseBeforeProjection(t *testing.T) {
	host := HostRecord{ID: "same-owner", MachineName: "garden", NameRevision: 1}
	if _, _, err := resolveDeclaredArgument("garden", []HostRecord{host, host}); err == nil || !strings.Contains(err.Error(), "duplicate host ID") {
		t.Fatalf("duplicate identity entered conflict suffix loop: %v", err)
	}
}
