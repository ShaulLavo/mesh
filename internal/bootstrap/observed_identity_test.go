package bootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestObservedDestinationPinJoinsReportedOwner(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "foreign"}[foreign], func(t *testing.T) {
			remote := &stubRemote{run: func(string, io.Reader) ([]byte, []byte, error) { return []byte("Linux\nx86_64\n"), nil, nil }}
			public := make([]byte, ed25519.PublicKeySize)
			public[0] = 1
			observed := base64.RawURLEncoding.EncodeToString(public)
			reported := observed
			if foreign {
				reported = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
			}
			deps := dependencies{
				localTailnet: func(context.Context) error { return nil }, servingPeer: noServingPeer, sshConfig: noSSHConfig,
				connect: func(context.Context, target, SSHOptions) (remoteHost, error) { return remote, nil },
				resolveBinary: func(context.Context, binarySelection, Platform) (resolvedBinary, error) {
					return resolvedBinary{path: "fixture-binary", cleanup: func() {}}, nil
				},
				checkClock:    func(context.Context, remoteHost, time.Time) error { return nil },
				authorizedKey: func(string) (string, error) { return "ssh-ed25519 adopter", nil },
				install:       func(context.Context, remoteHost, installRequest) (bool, error) { return true, nil },
				destination:   func(context.Context, remoteHost) (string, error) { return observed, nil },
				discover: func(context.Context, remoteHost) (tailscaleObservation, error) {
					return tailscaleObservation{State: tailscaleRunning, Tailnet: tailnetObservation{Name: "fixture.test", Addresses: []string{"127.0.0.1"}}}, nil
				},
				provision: func(_ context.Context, _ remoteHost, r provisionRequest) (provisionResult, error) {
					return provisionResult{Tailnet: r.Observation.Tailnet}, nil
				},
				verify: func(_ context.Context, _ []string, _ uint16, _ string, auth *transport.Authentication) (verifiedHost, string, error) {
					if auth.ExpectedIdentity != observed {
						t.Fatal("verification did not pin observed destination")
					}
					return verifiedHost{ID: reported, MeshIdentity: reported, MachineName: "forged", NameRevision: 99, TailscaleName: "fixture.test"}, "ws://127.0.0.1:7337/mesh", nil
				}, now: time.Now,
			}
			result, err := run(t.Context(), Options{Target: "fixture@fixture.test", StateDir: t.TempDir()}, deps)
			if foreign {
				if err == nil || result.ID != "" || result.MachineName != "" {
					t.Fatalf("observed owner accepted foreign result: %+v %v", result, err)
				}
				assertDiagnosticCode(t, err, DiagnosticIdentity)
				return
			}
			if err != nil || result.ID != observed || result.AuthenticatedIdentity != observed {
				t.Fatalf("matching observed owner refused: %+v %v", result, err)
			}
		})
	}
}
