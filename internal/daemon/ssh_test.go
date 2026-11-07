package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/tailnet"
)

func TestRunStartsSSHOnlyOnDiscoveredTailnetAddresses(t *testing.T) {
	stateDir := compactSocketTempDir(t)
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
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{
			StateDir: stateDir, SSHPort: 2222,
			SSHSessionHandler: func(actualStateDir string) sshd.SessionHandler {
				return sshHandlerForDirectory(stateDir, actualStateDir)
			},
		}, options)
	}()

	configs := []sshd.Config{waitForSSHConfig(t, started), waitForSSHConfig(t, started)}
	wantEndpoints := map[netip.AddrPort]bool{
		netip.MustParseAddrPort("100.64.0.7:2222"):          false,
		netip.MustParseAddrPort("[fd7a:115c:a1e0::7]:2222"): false,
	}
	var identityID string
	for _, cfg := range configs {
		if cfg.Handler == nil {
			t.Fatal("SSH listener lost the application handler")
		}
		if status, err := cfg.Handler(ctx, sshd.Session{}); err != nil || status != 17 {
			t.Fatalf("SSH handler = %d, %v", status, err)
		}
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

func sshHandlerForDirectory(expected, actual string) sshd.SessionHandler {
	return func(context.Context, sshd.Session) (int, error) {
		if actual != expected {
			return 1, fmt.Errorf("SSH state directory = %q, want %q", actual, expected)
		}
		return 17, nil
	}
}

func TestRunSkipsSSHAndReportsMissingTailnetAddress(t *testing.T) {
	stateDir := compactSocketTempDir(t)
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
	stateDir := compactSocketTempDir(t)
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
	err := run(context.Background(), Config{StateDir: compactSocketTempDir(t), SSHPort: 2222}, runOptions{
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

func TestRunSSHReusesScopeMembershipSnapshot(t *testing.T) {
	fixture := newDaemonRunFixture(t)
	host, _, err := identity.LoadOrCreate(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	subject := writeSSHMembershipPolicy(t, fixture.stateDir, host.ID)
	configs := make(chan sshd.Config, 1)
	fixture.start(t, func(ctx context.Context) error {
		return run(ctx, Config{StateDir: fixture.stateDir, SSHPort: 2222}, runOptions{
			now:    func() time.Time { return catalogTestTime },
			bootID: func() string { return "boot-membership" },
			discoverSelf: func(context.Context) (tailnet.Peer, error) {
				return tailnet.Peer{Name: "pc.fixture.test", Addrs: []string{"100.64.0.7"}}, nil
			},
			reconcileInterval: time.Hour,
			serveSSH:          func(ctx context.Context, cfg sshd.Config) error { configs <- cfg; <-ctx.Done(); return nil },
		})
	})
	cfg := waitForSSHConfig(t, configs)
	assertSSHMembershipCached(t, cfg.ScopeMembership, fixture.stateDir, subject)
	policy := filepath.Join(fixture.stateDir, "access", "scopes.json")
	if err := os.WriteFile(policy, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ScopeMembership(subject); err == nil {
		t.Fatal("cached SSH membership accepted damaged policy")
	}
	if err := os.Remove(policy); err != nil {
		t.Fatal(err)
	}
	if registered, err := cfg.ScopeMembership(subject); err != nil || registered {
		t.Fatalf("removed policy retained registration: %v %v", registered, err)
	}
	fixture.stop(t)
}

func writeSSHMembershipPolicy(t *testing.T, state, target string) string {
	t.Helper()
	now := time.Now().UTC()
	policy := access.PolicyView{Version: 1, Target: target, Revision: 1, Entries: make([]access.Entry, 16)}
	for index := range policy.Entries {
		key := sha256.Sum256([]byte(fmt.Sprintf("ssh-membership-fixture-%d", index)))
		policy.Entries[index] = access.Entry{Subject: access.Subject(base64.RawURLEncoding.EncodeToString(key[:])), Incarnation: "00112233445566778899aabbccddeeff", Permissions: []string{"metadata"}, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(state, "access"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "access", "scopes.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return string(policy.Entries[0].Subject)
}

func assertSSHMembershipCached(t *testing.T, membership func(string) (bool, error), state, subject string) {
	t.Helper()
	check := func() {
		registered, err := membership(subject)
		if err != nil || !registered {
			t.Fatalf("expired scope registration was lost: %v %v", registered, err)
		}
	}
	retained := testing.AllocsPerRun(5, check)
	uncached := testing.AllocsPerRun(5, func() {
		registered, err := access.Registered(state, access.Subject(subject))
		if err != nil || !registered {
			t.Fatalf("uncached registration: %v %v", registered, err)
		}
	})
	t.Logf("SSH callback %.0f allocations; uncached parse %.0f allocations", retained, uncached)
	if retained >= uncached/2 {
		t.Fatal("SSH assembly reparses unchanged scoped policy")
	}
}
