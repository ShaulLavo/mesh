package bootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"reflect"
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
	deps := dependencies{
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
		discover: func(context.Context, remoteHost) (tailnetObservation, error) {
			return tailnetObservation{Name: "pi.tail.example", Addresses: []string{"100.64.0.8", "fd7a:115c:a1e0::8"}}, nil
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
		Progress: func(event Event) { steps = append(steps, event.Step) },
	}, deps)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if result.ID != hostID || result.Endpoint != "ws://100.64.0.8:7337/mesh" || !result.AlreadyConfigured {
		t.Fatalf("result = %#v", result)
	}
	if gotInstall.BinaryPath != "/tmp/mesh-linux-arm64" || gotInstall.AuthorizedKey != "ssh-ed25519 adopter" || gotInstall.DaemonPort != DefaultPort {
		t.Fatalf("install request = %#v", gotInstall)
	}
	wantSteps := []Step{StepConnect, StepDetect, StepTransfer, StepInstall, StepDiscover, StepVerify}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("steps = %v, want %v", steps, wantSteps)
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
		connect: func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
		resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
			return resolvedBinary{path: "/tmp/mesh", cleanup: func() {}}, nil
		},
		checkClock:    func(context.Context, remoteHost, time.Time) error { return nil },
		authorizedKey: func(string) (string, error) { return "ssh-ed25519 adopter", nil },
		install:       func(context.Context, remoteHost, installRequest) (bool, error) { return true, nil },
		discover: func(context.Context, remoteHost) (tailnetObservation, error) {
			return tailnetObservation{Name: "pc.tail.example", Addresses: []string{"100.64.0.1"}}, nil
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

type stubRemote struct {
	run    func(string, io.Reader) ([]byte, []byte, error)
	closed int
}

func (r *stubRemote) Run(_ context.Context, command string, stdin io.Reader) ([]byte, []byte, error) {
	return r.run(command, stdin)
}

func (r *stubRemote) Close() error {
	r.closed++
	return nil
}
