package bootstrap

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/transport"
)

func TestRootAccountRequiresExplicitAcknowledgementBeforeInstallation(t *testing.T) {
	controller, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "denied", true: "acknowledged"}[allow], func(t *testing.T) {
			stopped := errors.New("fixture stopped after account approval")
			remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) { return nil, nil, stopped }}
			deps := dependencies{
				localTailnet: func(context.Context) error { return nil }, servingPeer: noServingPeer, sshConfig: noSSHConfig,
				connect: func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
				account: func(context.Context, remoteHost) (string, error) { return "root", nil },
				resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
					t.Fatal("installed fixture")
					return resolvedBinary{}, nil
				},
				install: func(context.Context, remoteHost, installRequest) (bool, error) {
					t.Fatal("installed fixture")
					return false, nil
				},
				discover: func(context.Context, remoteHost) (tailscaleObservation, error) { return tailscaleObservation{}, nil },
				provision: func(context.Context, remoteHost, provisionRequest) (provisionResult, error) {
					return provisionResult{}, nil
				},
				checkClock: func(context.Context, remoteHost, time.Time) error { return nil },
				verify: func(context.Context, []string, uint16, string, *transport.Authentication) (verifiedHost, string, error) {
					return verifiedHost{}, "", nil
				},
				authorizedKey: func(string) (string, error) { return "", nil }, now: time.Now,
			}
			_, err := run(context.Background(), Options{Target: "root@fixture.invalid", StateDir: t.TempDir(), AllowRoot: allow, AdditionalDeviceIdentities: []string{controller.ID}}, deps)
			if allow && !errors.Is(err, stopped) {
				t.Fatalf("acknowledged root did not reach platform probe: %v", err)
			}
			if !allow && (err == nil || !strings.Contains(err.Error(), "--allow-root") || !strings.Contains(err.Error(), "every selected fleet controller") || errors.Is(err, stopped)) {
				t.Fatalf("root approval bypassed: %v", err)
			}
		})
	}
}

func TestDiscoveryCannotEstablishADestinationPin(t *testing.T) {
	_, ok := probeRunningDaemon(context.Background(), normalizedOptions{stateDir: t.TempDir()}, dependencies{servingPeer: noServingPeer})
	if ok {
		t.Fatal("unpinned destination adopted")
	}
}

func TestDestinationIdentityComesFromTrustedSSH(t *testing.T) {
	host, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != `"$HOME/.local/bin/mesh" device identity --json` {
			t.Fatalf("trusted probe command=%q", command)
		}
		return []byte(`{"id":"` + host.ID + `","account":"fixture","fingerprint":"fixture"}`), nil, nil
	}}
	got, err := trustedDestinationIdentity(context.Background(), remote)
	if err != nil || got != host.ID {
		t.Fatalf("destination pin=%q, %v", got, err)
	}
}

func TestAdditionalControllerValidationPrecedesRemoteAccess(t *testing.T) {
	valid, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, selected := range map[string][]string{
		"empty": {""}, "malformed": {"not-a-key"}, "noncanonical": {valid.ID + "="},
		"invalid after valid": {valid.ID, "invalid"},
		"too many":            slices.Repeat([]string{valid.ID}, maximumAdditionalDevices+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t.Context(), Options{Target: "root@fixture.invalid", StateDir: t.TempDir(), AdditionalDeviceIdentities: selected}, dependencies{
				connect: func(context.Context, target, SSHOptions) (remoteHost, error) {
					t.Fatal("invalid selection reached SSH")
					return nil, nil
				},
			})
			assertDiagnosticCode(t, err, DiagnosticIdentity)
		})
	}
}

func TestAdditionalControllerLimitAndSelectionDoNotMutateCaller(t *testing.T) {
	state := t.TempDir()
	source, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	selected := slices.Repeat([]string{source.ID}, maximumAdditionalDevices)
	before := slices.Clone(selected)
	got, err := normalizeAdditionalDeviceIdentities(state, selected)
	if err != nil || len(got) != 0 || !slices.Equal(selected, before) {
		t.Fatalf("source deduplication = %v, %v; selection changed=%t", got, err, !slices.Equal(selected, before))
	}
}
