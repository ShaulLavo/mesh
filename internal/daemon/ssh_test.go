package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/tailnet"
)

func TestRunStartsSSHOnlyOnDiscoveredTailnetAddresses(t *testing.T) {
	stateDir := t.TempDir()
	started := make(chan sshd.Config, 2)
	options := runOptions{
		now:    func() time.Time { return catalogTestTime },
		bootID: func() string { return "boot-a" },
		discoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{
				Name: "pc.example.ts.net",
				Addrs: []string{
					"fd7a:115c:a1e0::7",
					"100.64.0.7",
				},
			}, nil
		},
		reconcileInterval: time.Hour,
		serveSSH: func(ctx context.Context, cfg sshd.Config) error {
			started <- cfg
			<-ctx.Done()
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{StateDir: stateDir, SSHPort: 2222}, options)
	}()

	configs := []sshd.Config{waitForSSHConfig(t, started), waitForSSHConfig(t, started)}
	wantEndpoints := map[netip.AddrPort]bool{
		netip.MustParseAddrPort("100.64.0.7:2222"):          false,
		netip.MustParseAddrPort("[fd7a:115c:a1e0::7]:2222"): false,
	}
	var identityID string
	for _, cfg := range configs {
		endpoint, err := netip.ParseAddrPort(cfg.Addr)
		if err != nil {
			t.Fatalf("SSH endpoint %q: %v", cfg.Addr, err)
		}
		if _, ok := wantEndpoints[endpoint]; !ok {
			t.Fatalf("SSH endpoint = %s, want discovered Tailnet addresses", endpoint)
		}
		wantEndpoints[endpoint] = true
		if endpoint.Addr().IsLoopback() || endpoint.Addr().IsUnspecified() {
			t.Fatalf("SSH endpoint is not Tailnet-scoped: %s", endpoint)
		}
		if cfg.AuthorizedKeys != filepath.Join(stateDir, "authorized_keys") {
			t.Fatalf("authorized_keys = %q", cfg.AuthorizedKeys)
		}
		currentID := base64.RawURLEncoding.EncodeToString(cfg.HostKey.Public().(ed25519.PublicKey))
		if identityID == "" {
			identityID = currentID
		} else if currentID != identityID {
			t.Fatalf("SSH listeners use different host identities: %q and %q", identityID, currentID)
		}
	}
	for endpoint, observed := range wantEndpoints {
		if !observed {
			t.Fatalf("SSH listener was not started on %s", endpoint)
		}
	}

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRunSkipsSSHAndReportsMissingTailnetAddress(t *testing.T) {
	stateDir := t.TempDir()
	reported := make(chan error, 1)
	serveCalled := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{
			StateDir: stateDir,
			SSHPort:  2222,
			ReportError: func(err error) {
				reported <- err
			},
		}, runOptions{
			now:               func() time.Time { return catalogTestTime },
			bootID:            func() string { return "boot-a" },
			discoverSelf:      func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, nil },
			reconcileInterval: time.Hour,
			serveSSH: func(context.Context, sshd.Config) error {
				serveCalled <- struct{}{}
				return nil
			},
		})
	}()
	client := dialUnixRuntime(t, SocketPath(stateDir))
	_ = client.Close()
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "SSH listener disabled") || !strings.Contains(err.Error(), "no Tailscale addresses") {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("missing Tailnet address was not reported")
	}
	select {
	case <-serveCalled:
		t.Fatal("SSH server started without a Tailnet address")
	default:
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRunSkipsSSHOutsideTailscaleRanges(t *testing.T) {
	stateDir := t.TempDir()
	reported := make(chan error, 1)
	serveCalled := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{
			StateDir: stateDir,
			SSHPort:  2222,
			ReportError: func(err error) {
				reported <- err
			},
		}, runOptions{
			now:               func() time.Time { return catalogTestTime },
			bootID:            func() string { return "boot-a" },
			discoverSelf:      func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{Addrs: []string{"127.0.0.1"}}, nil },
			reconcileInterval: time.Hour,
			serveSSH: func(context.Context, sshd.Config) error {
				serveCalled <- struct{}{}
				return nil
			},
		})
	}()
	client := dialUnixRuntime(t, SocketPath(stateDir))
	_ = client.Close()
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "SSH listener disabled") || !strings.Contains(err.Error(), "outside the Tailscale") {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("foreign Tailnet address was not reported")
	}
	select {
	case <-serveCalled:
		t.Fatal("SSH server started outside the Tailscale address ranges")
	default:
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRunTreatsSSHListenerFailureAsDaemonFailure(t *testing.T) {
	serveErr := errors.New("SSH port is occupied")
	err := run(context.Background(), Config{StateDir: t.TempDir(), SSHPort: 2222}, runOptions{
		now:               func() time.Time { return catalogTestTime },
		bootID:            func() string { return "boot-a" },
		discoverSelf:      func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{Addrs: []string{"100.64.0.7"}}, nil },
		reconcileInterval: time.Hour,
		serveSSH:          func(context.Context, sshd.Config) error { return serveErr },
	})
	if !errors.Is(err, serveErr) || !strings.Contains(err.Error(), "100.64.0.7:2222") {
		t.Fatalf("Run error = %v", err)
	}
}

func waitForSSHConfig(t *testing.T, started <-chan sshd.Config) sshd.Config {
	t.Helper()
	select {
	case cfg := <-started:
		return cfg
	case <-time.After(runtimeTestTimeout):
		t.Fatal("SSH listener did not start")
		return sshd.Config{}
	}
}
