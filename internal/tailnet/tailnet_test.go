package tailnet

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type runFunc func(context.Context, string, ...string) ([]byte, []byte, error)

func (f runFunc) Run(ctx context.Context, command string, args ...string) ([]byte, []byte, error) {
	return f(ctx, command, args...)
}

func TestClientParsesTailscaleStatus(t *testing.T) {
	fixture := readFixture(t, "status-running.json")
	runner := runFunc(func(_ context.Context, command string, args ...string) ([]byte, []byte, error) {
		if command != "tailscale" {
			t.Fatalf("command = %q, want tailscale", command)
		}
		if !slices.Equal(args, []string{"status", "--json"}) {
			t.Fatalf("arguments = %q, want status --json", args)
		}
		return fixture, []byte("a harmless stderr diagnostic\n"), nil
	})
	client := NewClient(runner)

	self, err := client.Self(context.Background())
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	wantSelf := Peer{
		Name:   "desktop.example.ts.net",
		Addrs:  []string{"100.101.102.103", "fd7a:115c:a1e0::1"},
		Online: true,
	}
	assertPeer(t, self, wantSelf)

	peers, err := client.Peers(context.Background())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	wantPeers := []Peer{
		{Name: "laptop.example.ts.net", Addrs: []string{"100.88.77.66", "fd7a:115c:a1e0::2"}, Online: true},
		{Name: "pi.example.ts.net", Addrs: []string{"100.64.0.9"}, Online: false},
	}
	if len(peers) != len(wantPeers) {
		t.Fatalf("peer count = %d, want %d: %#v", len(peers), len(wantPeers), peers)
	}
	for i := range wantPeers {
		assertPeer(t, peers[i], wantPeers[i])
	}
}

func TestClientExplainsTailscaleFailures(t *testing.T) {
	tests := []struct {
		name       string
		stdout     []byte
		stderr     []byte
		runErr     error
		want       error
		wantAdvice string
	}{
		{
			name:       "not installed",
			runErr:     &exec.Error{Name: "tailscale", Err: exec.ErrNotFound},
			want:       ErrNotInstalled,
			wantAdvice: "install Tailscale",
		},
		{
			name:       "daemon not running",
			stderr:     readFixture(t, "daemon-not-running.txt"),
			runErr:     errors.New("exit status 1"),
			want:       ErrNotRunning,
			wantAdvice: "start Tailscale",
		},
		{
			name:       "stopped",
			stdout:     readFixture(t, "status-stopped.json"),
			want:       ErrNotRunning,
			wantAdvice: "tailscale up",
		},
		{
			name:       "logged out",
			stdout:     readFixture(t, "status-needs-login.json"),
			want:       ErrNotLoggedIn,
			wantAdvice: "tailscale up",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(runFunc(func(context.Context, string, ...string) ([]byte, []byte, error) {
				return tt.stdout, tt.stderr, tt.runErr
			}))
			_, err := client.Self(context.Background())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Self error = %v, want errors.Is(%v)", err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.wantAdvice) {
				t.Fatalf("Self error = %q, want advice containing %q", err, tt.wantAdvice)
			}
		})
	}
}

func TestSelfReportsMissingTailscaleFromDefaultRunner(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Self(context.Background())
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Self error = %v, want ErrNotInstalled", err)
	}
}

func TestClientRejectsInvalidStatusAtBoundary(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{name: "malformed JSON", status: `{`, want: "parse tailscale status"},
		{
			name:   "missing self",
			status: `{"BackendState":"Running","Self":null}`,
			want:   "does not identify this machine",
		},
		{
			name:   "invalid address",
			status: `{"BackendState":"Running","Self":{"DNSName":"box.example.ts.net.","TailscaleIPs":["not-an-ip"],"Online":true}}`,
			want:   "invalid Tailscale address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(runFunc(func(context.Context, string, ...string) ([]byte, []byte, error) {
				return []byte(tt.status), nil, nil
			}))
			_, err := client.Self(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Self error = %v, want text %q", err, tt.want)
			}
		})
	}
}

func TestClientPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewClient(runFunc(func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, nil, context.Canceled
	}))

	_, err := client.Self(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Self error = %v, want context cancellation", err)
	}
}

func TestExecRunnerPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := (execRunner{}).Run(ctx, "/bin/sh", "-c", "exit 0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("exec runner error = %v, want context cancellation", err)
	}
}

func TestExecRunnerPreservesContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, _, err := (execRunner{}).Run(ctx, "/bin/sh", "-c", "while :; do :; done")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec runner error = %v, want context deadline", err)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func assertPeer(t *testing.T, got, want Peer) {
	t.Helper()
	if got.Name != want.Name || got.Online != want.Online || !slices.Equal(got.Addrs, want.Addrs) {
		t.Fatalf("peer = %#v, want %#v", got, want)
	}
}
