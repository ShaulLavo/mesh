package bootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunCompletesEveryBoundaryAndReturnsVerifiedHost(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != "uname -s; uname -m" {
			t.Fatalf("unexpected remote command %q", command)
		}
		return []byte("Linux\naarch64\n"), nil, nil
	}}
	hostID := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	var gotInstall installRequest
	var steps []Step
	foundVariant := ""
	deps := dependencies{
		localTailnet: func(context.Context) error { return nil },
		connect: func(_ context.Context, got target, _ SSHOptions) (remoteHost, error) {
			if got.display() != "shaul@pi" {
				t.Fatalf("target = %s", got.display())
			}
			return remote, nil
		},
		resolveBinary: func(_ context.Context, _ binarySelection, got Platform) (resolvedBinary, error) {
			if got != (Platform{OS: Linux, Arch: ARM64}) {
				t.Fatalf("platform = %#v", got)
			}
			return resolvedBinary{path: "/tmp/mesh-linux-arm64", cleanup: func() {}}, nil
		},
		checkClock: func(context.Context, remoteHost, time.Time) error { return nil },
		authorizedKey: func(string) (string, error) {
			return "ssh-ed25519 adopter", nil
		},
		install: func(_ context.Context, _ remoteHost, request installRequest) (bool, error) {
			gotInstall = request
			return true, nil
		},
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
			return tailscaleObservation{State: tailscaleRunning, Variant: tailscaleVariantSystem, Tailnet: tailnetObservation{Name: "pi.tail.example", Addresses: []string{"100.64.0.8", "fd7a:115c:a1e0::8"}}}, nil
		},
		provision: func(_ context.Context, _ remoteHost, request provisionRequest) (provisionResult, error) {
			if request.Observation.State != tailscaleRunning {
				t.Fatalf("provision observation = %#v", request.Observation)
			}
			return provisionResult{Tailnet: request.Observation.Tailnet}, nil
		},
		verify: func(_ context.Context, addresses []string, port uint16, websocketPath string) (verifiedHost, string, error) {
			if !reflect.DeepEqual(addresses, []string{"100.64.0.8", "fd7a:115c:a1e0::8"}) || port != DefaultPort || websocketPath != DefaultWebSocketPath {
				t.Fatalf("verify args = %v, %d, %s", addresses, port, websocketPath)
			}
			return verifiedHost{ID: hostID, MeshIdentity: hostID, TailscaleName: "pi.tail.example"}, "ws://100.64.0.8:7337/mesh", nil
		},
		now: func() time.Time { return time.Unix(100, 0) },
	}

	result, err := run(context.Background(), Options{
		Target:   "shaul@pi",
		StateDir: t.TempDir(),
		Progress: func(event Event) {
			steps = append(steps, event.Step)
			if event.Detail == "found headless Tailscale daemon" {
				foundVariant = event.Detail
			}
		},
	}, deps)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if result.ID != hostID || result.Endpoint != "ws://100.64.0.8:7337/mesh" || !result.AlreadyConfigured {
		t.Fatalf("result = %#v", result)
	}
	if gotInstall.BinaryPath != "/tmp/mesh-linux-arm64" || gotInstall.AuthorizedKey != "ssh-ed25519 adopter" || gotInstall.DaemonPort != DefaultPort || gotInstall.SSHPort != DefaultSSHPort {
		t.Fatalf("install request = %#v", gotInstall)
	}
	wantSteps := []Step{StepConnect, StepDetect, StepDiscover, StepDiscover, StepTransfer, StepInstall, StepVerify}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("steps = %v, want %v", steps, wantSteps)
	}
	if foundVariant == "" {
		t.Fatal("run did not report the detected Tailscale variant")
	}
	if remote.closed != 1 {
		t.Fatalf("remote close count = %d", remote.closed)
	}
}

func TestRunRefusesChangedPinnedIdentity(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) {
		return []byte("Linux\nx86_64\n"), nil, nil
	}}
	hostID := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	pinnedKey := make([]byte, ed25519.PublicKeySize)
	pinnedKey[0] = 1
	pinnedID := base64.RawURLEncoding.EncodeToString(pinnedKey)
	deps := dependencies{
		localTailnet: func(context.Context) error { return nil },
		connect:      func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
			return resolvedBinary{path: "/tmp/mesh", cleanup: func() {}}, nil
		},
		checkClock:    func(context.Context, remoteHost, time.Time) error { return nil },
		authorizedKey: func(string) (string, error) { return "ssh-ed25519 adopter", nil },
		install:       func(context.Context, remoteHost, installRequest) (bool, error) { return true, nil },
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
			return tailscaleObservation{State: tailscaleRunning, Tailnet: tailnetObservation{Name: "pc.tail.example", Addresses: []string{"100.64.0.1"}}}, nil
		},
		provision: func(_ context.Context, _ remoteHost, request provisionRequest) (provisionResult, error) {
			return provisionResult{Tailnet: request.Observation.Tailnet}, nil
		},
		verify: func(context.Context, []string, uint16, string) (verifiedHost, string, error) {
			return verifiedHost{ID: hostID, MeshIdentity: hostID, TailscaleName: "pc.tail.example"}, "ws://100.64.0.1:7337/mesh", nil
		},
		now: time.Now,
	}
	_, err := run(context.Background(), Options{
		Target:           "shaul@pc",
		StateDir:         t.TempDir(),
		ExpectedIdentity: pinnedID,
	}, deps)
	assertDiagnosticCode(t, err, DiagnosticIdentity)
}

func TestRunProvisionFailurePrecedesBinaryTransferAndMeshInstall(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != "uname -s; uname -m" {
			t.Fatalf("remote write ran after failed provisioning: %q", command)
		}
		return []byte("Linux\naarch64\n"), nil, nil
	}}
	provisionFailure := diagnostic(DiagnosticTailscaleUnavailable, errors.New("fixture provision failure"))
	deps := dependencies{
		localTailnet: func(context.Context) error { return nil },
		connect:      func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
			t.Fatal("binary resolution ran before Tailscale provisioning succeeded")
			return resolvedBinary{}, nil
		},
		install: func(context.Context, remoteHost, installRequest) (bool, error) {
			t.Fatal("Mesh installer ran after Tailscale provisioning failed")
			return false, nil
		},
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
			return tailscaleObservation{State: tailscaleMissing}, nil
		},
		provision: func(_ context.Context, _ remoteHost, request provisionRequest) (provisionResult, error) {
			if request.Platform != (Platform{OS: Linux, Arch: ARM64}) {
				t.Fatalf("provision platform = %#v", request.Platform)
			}
			return provisionResult{}, provisionFailure
		},
		checkClock: func(context.Context, remoteHost, time.Time) error {
			t.Fatal("clock check ran after failed provisioning")
			return nil
		},
		verify: func(context.Context, []string, uint16, string) (verifiedHost, string, error) {
			t.Fatal("verification ran after failed provisioning")
			return verifiedHost{}, "", nil
		},
		authorizedKey: func(string) (string, error) { t.Fatal("identity load ran after failed provisioning"); return "", nil },
		now:           time.Now,
	}
	_, err := run(context.Background(), Options{Target: "alice@pi", StateDir: t.TempDir()}, deps)
	if !errors.Is(err, provisionFailure) {
		t.Fatalf("run() error = %v, want provision failure", err)
	}
}

func TestRunRedactsAuthKeyFromAnyReturnedDiagnostic(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-run-redaction-fixture" //nolint:gosec // inert sentinel must not survive the Run boundary
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != "uname -s; uname -m" {
			t.Fatalf("unexpected remote command %q", command)
		}
		return []byte("Linux\nx86_64\n"), nil, nil
	}}
	deps := dependencies{
		localTailnet:  func(context.Context) error { return nil },
		connect:       func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) { return resolvedBinary{}, nil },
		install:       func(context.Context, remoteHost, installRequest) (bool, error) { return false, nil },
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
			return tailscaleObservation{State: tailscaleNeedsLogin}, nil
		},
		provision: func(context.Context, remoteHost, provisionRequest) (provisionResult, error) {
			return provisionResult{}, diagnostic(DiagnosticTailscaleLoggedOut, errors.New("remote echoed "+secret))
		},
		checkClock: func(context.Context, remoteHost, time.Time) error { return nil },
		verify: func(context.Context, []string, uint16, string) (verifiedHost, string, error) {
			return verifiedHost{}, "", nil
		},
		authorizedKey: func(string) (string, error) { return "", nil },
		now:           time.Now,
	}
	_, err := run(context.Background(), Options{Target: "alice@pi", StateDir: t.TempDir(), TailscaleAuthKey: []byte(secret)}, deps)
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunRedactsAuthKeyFromOptionValidation(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-option-redaction-fixture" //nolint:gosec // inert sentinel must not survive the Run boundary
	_, err := run(context.Background(), Options{
		Target: "alice@pi", StateDir: t.TempDir(), ExpectedIdentity: secret, TailscaleAuthKey: []byte(secret),
	}, dependencies{})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunDiscardsResultContainingAuthKey(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-result-redaction-fixture" //nolint:gosec // inert sentinel must not survive the Run boundary
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != "uname -s; uname -m" {
			t.Fatalf("unexpected remote command %q", command)
		}
		return []byte("Linux\nx86_64\n"), nil, nil
	}}
	hostID := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	deps := dependencies{
		localTailnet: func(context.Context) error { return nil },
		connect:      func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
			return resolvedBinary{path: "/tmp/mesh", cleanup: func() {}}, nil
		},
		install: func(context.Context, remoteHost, installRequest) (bool, error) { return true, nil },
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
			return tailscaleObservation{State: tailscaleRunning, Tailnet: tailnetObservation{Name: "pi.tail.example", Addresses: []string{"100.64.0.8"}}}, nil
		},
		provision: func(_ context.Context, _ remoteHost, request provisionRequest) (provisionResult, error) {
			return provisionResult{Tailnet: request.Observation.Tailnet}, nil
		},
		checkClock:    func(context.Context, remoteHost, time.Time) error { return nil },
		authorizedKey: func(string) (string, error) { return "ssh-ed25519 adopter", nil },
		verify: func(context.Context, []string, uint16, string) (verifiedHost, string, error) {
			return verifiedHost{ID: hostID, MeshIdentity: hostID, TailscaleName: "pi.tail.example"}, "ws://" + secret + ":7337/mesh", nil
		},
		now: time.Now,
	}
	result, err := run(context.Background(), Options{Target: "alice@pi", StateDir: t.TempDir(), TailscaleAuthKey: []byte(secret)}, deps)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if !reflect.DeepEqual(result, Result{}) || strings.Contains(err.Error(), secret) {
		t.Fatalf("run() = %#v, %v", result, err)
	}
}

func TestRunRejectsAuthKeyInTailnetStatusBeforeMeshWrite(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-status-result-fixture" //nolint:gosec // inert sentinel must not survive the Run boundary
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if command != "uname -s; uname -m" {
			t.Fatalf("unexpected remote command %q", command)
		}
		return []byte("Linux\nx86_64\n"), nil, nil
	}}
	deps := dependencies{
		localTailnet: func(context.Context) error { return nil },
		connect:      func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
			return tailscaleObservation{State: tailscaleRunning, Tailnet: tailnetObservation{Name: secret, Addresses: []string{"100.64.0.8"}}}, nil
		},
		provision: func(_ context.Context, _ remoteHost, request provisionRequest) (provisionResult, error) {
			return provisionResult{Tailnet: request.Observation.Tailnet}, nil
		},
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
			t.Fatal("binary resolution ran after contaminated Tailscale status")
			return resolvedBinary{}, nil
		},
		install: func(context.Context, remoteHost, installRequest) (bool, error) {
			t.Fatal("Mesh installer ran after contaminated Tailscale status")
			return false, nil
		},
		checkClock:    func(context.Context, remoteHost, time.Time) error { return nil },
		authorizedKey: func(string) (string, error) { return "ssh-ed25519 adopter", nil },
		verify: func(context.Context, []string, uint16, string) (verifiedHost, string, error) {
			return verifiedHost{}, "", nil
		},
		now: time.Now,
	}
	result, err := run(context.Background(), Options{Target: "alice@pi", StateDir: t.TempDir(), TailscaleAuthKey: []byte(secret)}, deps)
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if !reflect.DeepEqual(result, Result{}) || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "Mesh was not installed") {
		t.Fatalf("run() = %#v, %v", result, err)
	}
}

func TestRemoteCommandErrorBoundsUntrustedOutput(t *testing.T) {
	t.Parallel()

	detail := strings.Repeat("x", 32<<10)
	err := remoteCommandError("fixture operation", errors.New("exit 1"), nil, []byte(detail))
	if len(err.Error()) >= len(detail) || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("remoteCommandError() returned %d bytes", len(err.Error()))
	}
}

type stubRemote struct {
	run           func(string, io.Reader) ([]byte, []byte, error)
	runWithLimits func(string, io.Reader, []remoteOutputLimits) ([]byte, []byte, error)
	closed        int
}

func (r *stubRemote) Run(_ context.Context, command string, stdin io.Reader, limits ...remoteOutputLimits) ([]byte, []byte, error) {
	if r.runWithLimits != nil {
		return r.runWithLimits(command, stdin, limits)
	}
	return r.run(command, stdin)
}

func (r *stubRemote) Close() error {
	r.closed++
	return nil
}

func TestRunChecksThisMachineBeforeTouchingTheRemote(t *testing.T) {
	t.Parallel()

	// Adoption dials the host over the tailnet from here, so a machine that is
	// not on it can never finish. Discovering that after installing Tailscale on
	// someone else's computer is the failure this guards.
	connected := false
	_, err := run(context.Background(), Options{
		Target: "alice@pi", StateDir: t.TempDir(),
	}, dependencies{
		localTailnet: func(context.Context) error { return errors.New("tailscale is not installed") },
		connect: func(context.Context, target, SSHOptions) (remoteHost, error) {
			connected = true
			return nil, errors.New("must not connect")
		},
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
			return resolvedBinary{}, nil
		},
		install:  func(context.Context, remoteHost, installRequest) (bool, error) { return false, nil },
		discover: func(context.Context, remoteHost) (tailscaleObservation, error) { return tailscaleObservation{}, nil },
		provision: func(context.Context, remoteHost, provisionRequest) (provisionResult, error) {
			return provisionResult{}, nil
		},
		checkClock: func(context.Context, remoteHost, time.Time) error { return nil },
		verify: func(context.Context, []string, uint16, string) (verifiedHost, string, error) {
			return verifiedHost{}, "", nil
		},
		authorizedKey: func(string) (string, error) { return "ssh-ed25519 AAAA", nil },
		now:           time.Now,
	})
	assertDiagnosticCode(t, err, DiagnosticLocalTailscale)
	if connected {
		t.Fatal("connected to the remote host before checking this machine")
	}
}
