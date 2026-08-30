package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/worker"
)

func TestConfigureTailscaleServeUsesExactBoundedCommand(t *testing.T) {
	var gotName string
	var gotArguments []string
	err := configureTailscaleServe(context.Background(), 8443, time.Second, func(ctx context.Context, name string, arguments ...string) ([]byte, error) {
		gotName = name
		gotArguments = append([]string(nil), arguments...)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			return nil, errors.New("command context is not bounded")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{"serve", "--bg", "--yes", "--tcp=443", "tcp://127.0.0.1:8443"}
	if gotName != "tailscale" || !reflect.DeepEqual(gotArguments, wantArguments) {
		t.Fatalf("command = %s %#v, want tailscale %#v", gotName, gotArguments, wantArguments)
	}
}

func TestConfigureTailscaleServePropagatesTimeoutCancellationAndFailure(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		err := configureTailscaleServe(context.Background(), 8443, 25*time.Millisecond, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := configureTailscaleServe(ctx, 8443, time.Second, func(context.Context, string, ...string) ([]byte, error) {
			called = true
			return nil, nil
		})
		if !errors.Is(err, context.Canceled) || called {
			t.Fatalf("cancellation error = %v, runner called = %v", err, called)
		}
	})

	t.Run("command failure", func(t *testing.T) {
		err := configureTailscaleServe(context.Background(), 8443, time.Second, func(context.Context, string, ...string) ([]byte, error) {
			return []byte("operator permission is missing\n"), errors.New("exit status 1")
		})
		if err == nil || !strings.Contains(err.Error(), "operator permission is missing") || !strings.Contains(err.Error(), "exit status 1") {
			t.Fatalf("command failure = %v", err)
		}
	})
}

func TestRunExternalCommandBoundsCancellationWithInheritedPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runExternalCommand(ctx, "/bin/sh", "-c", "sleep 2 & wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inherited-pipe cancellation error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > externalCommandWaitDelay+time.Second {
		t.Fatalf("inherited-pipe cancellation took %s", elapsed)
	}
}

func TestRunConfiguresTailscaleServeAfterLocalListenersAreReady(t *testing.T) {
	stateDir := t.TempDir()
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	controlPort := reserveTCPPort(t, "127.0.0.1")
	signerID, _ := composedIdentity(t)
	configured := make(chan error, 1)
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Name: "origin.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(_ context.Context, name string, arguments ...string) ([]byte, error) {
		if name != "tailscale" || !reflect.DeepEqual(arguments, []string{"serve", "--bg", "--yes", "--tcp=443", fmt.Sprintf("tcp://127.0.0.1:%d", httpsPort)}) {
			return nil, fmt.Errorf("unexpected command %s %#v", name, arguments)
		}
		if _, err := os.Lstat(SocketPath(stateDir)); err != nil {
			return nil, fmt.Errorf("Unix listener was not ready: %w", err)
		}
		connection, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", httpsPort), time.Second)
		if err == nil {
			_ = connection.Close()
		}
		if err == nil {
			control, controlErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", controlPort), time.Second)
			if controlErr == nil {
				_ = control.Close()
			}
			if controlErr != nil {
				err = fmt.Errorf("Tailnet control listener was not ready: %w", controlErr)
			}
		}
		configured <- err
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{
			StateDir: stateDir, TailnetPort: controlPort, HTTPSPort: httpsPort, CertificateRenewerID: signerID, TailscaleServe: true,
		}, options)
	}()
	select {
	case err := <-configured:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("Tailscale Serve was not configured")
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRunFailsWhenTailscaleServeConfigurationFails(t *testing.T) {
	stateDir := t.TempDir()
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	controlPort := reserveTCPPort(t, "127.0.0.1")
	signerID, _ := composedIdentity(t)
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Name: "origin.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("run tailscale set --operator first"), errors.New("exit status 1")
	}
	err := run(context.Background(), Config{
		StateDir: stateDir, TailnetPort: controlPort, HTTPSPort: httpsPort, CertificateRenewerID: signerID, TailscaleServe: true,
	}, options)
	if err == nil || !strings.Contains(err.Error(), "tailscale set --operator") {
		t.Fatalf("daemon Tailscale Serve failure = %v", err)
	}
	if _, statErr := os.Lstat(SocketPath(stateDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon socket after Tailscale Serve failure: %v", statErr)
	}
}

func TestRunRejectsTailscaleServeControlPortConflict(t *testing.T) {
	err := run(context.Background(), Config{
		StateDir: t.TempDir(), TailnetPort: 443, HTTPSPort: 8443, TailscaleServe: true,
	}, defaultRunOptions())
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("Tailscale Serve port conflict = %v", err)
	}
}

func TestRunRejectsTailscaleServeWithoutControlListener(t *testing.T) {
	err := run(context.Background(), Config{
		StateDir: t.TempDir(), HTTPSPort: 8443, TailscaleServe: true,
	}, defaultRunOptions())
	if err == nil || !strings.Contains(err.Error(), "control port") {
		t.Fatalf("Tailscale Serve without control listener = %v", err)
	}
}

func TestRunRejectsTailscaleServeWithoutEligibleAddressesBeforeCommand(t *testing.T) {
	discoveryErr := errors.New("tailscaled unavailable")
	tests := []struct {
		name     string
		peer     tailnet.Peer
		discover error
		contains string
	}{
		{name: "discovery failure", discover: discoveryErr, contains: "tailscaled unavailable"},
		{name: "empty", peer: tailnet.Peer{}, contains: "at least one discovered"},
		{name: "IPv6 only", peer: tailnet.Peer{Addrs: []string{"fd7a:115c:a1e0::1"}}, contains: "100.64.0.0/10"},
		{name: "non-Tailscale IPv4", peer: tailnet.Peer{Addrs: []string{"192.0.2.1"}}, contains: "outside the Tailscale"},
		{name: "mixed valid and foreign", peer: tailnet.Peer{Addrs: []string{"100.64.0.1", "203.0.113.10"}}, contains: "outside the Tailscale"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			options := defaultRunOptions()
			options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
				return test.peer, test.discover
			}
			options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
				called = true
				return nil, nil
			}
			err := run(context.Background(), Config{
				StateDir: t.TempDir(), TailnetPort: 7337, HTTPSPort: 8443, TailscaleServe: true,
			}, options)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("startup error = %v, want text %q", err, test.contains)
			}
			if called {
				t.Fatal("Tailscale Serve command ran without an eligible control address")
			}
		})
	}
}

func TestRunBoundsInitialTailscaleServeDiscovery(t *testing.T) {
	called := false
	options := defaultRunOptions()
	options.tailnetDiscoveryTimeout = 5 * time.Millisecond
	options.discoverSelf = func(ctx context.Context) (tailnet.Peer, error) {
		<-ctx.Done()
		return tailnet.Peer{}, ctx.Err()
	}
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	err := run(context.Background(), Config{
		StateDir: t.TempDir(), TailnetPort: 7337, HTTPSPort: 8443, TailscaleServe: true,
	}, options)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded startup discovery error = %v", err)
	}
	if called {
		t.Fatal("Tailscale Serve command ran after discovery timed out")
	}
}

func TestRunTailscaleServeRequiresEveryControlAddressToBind(t *testing.T) {
	blocked, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close() //nolint:errcheck // test cleanup
	controlPort := uint16(blocked.Addr().(*net.TCPAddr).Port)
	signerID, _ := composedIdentity(t)
	called := false
	options := defaultRunOptions()
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Addrs: []string{"127.0.0.1", "127.0.0.2"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	err = run(context.Background(), Config{
		StateDir: t.TempDir(), TailnetPort: controlPort, HTTPSPort: reserveTCPPort(t, "127.0.0.1"),
		CertificateRenewerID: signerID, TailscaleServe: true,
	}, options)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "bind Tailnet") {
		t.Fatalf("partial control bind error = %v", err)
	}
	if called {
		t.Fatal("Tailscale Serve command ran after a partial control-listener bind")
	}
}

func TestRunAllowsEqualHTTPSAndControlPortsOnSeparateAddresses(t *testing.T) {
	port := reserveTCPPort(t, "127.0.0.1")
	signerID, _ := composedIdentity(t)
	ready := make(chan struct{}, 1)
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Addrs: []string{"127.0.0.2"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		ready <- struct{}{}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{
			StateDir: t.TempDir(), TailnetPort: port, HTTPSPort: port,
			CertificateRenewerID: signerID, TailscaleServe: true,
		}, options)
	}()
	select {
	case <-ready:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("equal-port daemon did not become ready")
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatalf("equal HTTPS/control ports on separate addresses: %v", err)
	}
}

func TestRunRestartsOnTailnetAddressChangeAndPreservesWorkerState(t *testing.T) {
	stateDir := t.TempDir()
	sessionDir := filepath.Join(stateDir, sessionsDirectoryName, "R7T2")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	exitedAt := catalogTestTime.Add(time.Minute)
	if err := worker.WriteMeta(sessionDir, worker.Meta{
		ID: "R7T2", PID: os.Getpid(), Command: []string{"detached-worker"}, Cwd: stateDir,
		State: worker.StateExited, CreatedAt: catalogTestTime, ExitedAt: &exitedAt, ExitCode: &exitCode,
	}); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(sessionDir, "meta.json")
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	controlPort := reserveTCPPort(t, "127.0.0.1")
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	signerID, _ := composedIdentity(t)
	config := Config{
		StateDir: stateDir, TailnetPort: controlPort, HTTPSPort: httpsPort,
		CertificateRenewerID: signerID, TailscaleServe: true,
	}
	var discoveryCalls atomic.Int32
	firstOptions := defaultRunOptions()
	firstOptions.reconcileInterval = time.Hour
	firstOptions.tailnetPollInterval = 10 * time.Millisecond
	firstOptions.validateServeAddresses = func([]string) error { return nil }
	firstOptions.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		if discoveryCalls.Add(1) == 1 {
			return tailnet.Peer{Addrs: []string{"127.0.0.1"}}, nil
		}
		return tailnet.Peer{Addrs: []string{"127.0.0.2"}}, nil
	}
	firstOptions.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	firstDone := make(chan error, 1)
	go func() { firstDone <- run(context.Background(), config, firstOptions) }()
	firstErr := waitRuntime(t, firstDone)
	if !errors.Is(firstErr, ErrTailnetAddressesChanged) {
		t.Fatalf("first daemon error = %v, want address-change restart", firstErr)
	}
	if connection, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", controlPort), 50*time.Millisecond); dialErr == nil {
		_ = connection.Close()
		t.Fatal("old control endpoint remained bound after address-change shutdown")
	}

	secondReady := make(chan struct{}, 1)
	secondOptions := defaultRunOptions()
	secondOptions.reconcileInterval = time.Hour
	secondOptions.tailnetPollInterval = 10 * time.Millisecond
	secondOptions.validateServeAddresses = func([]string) error { return nil }
	secondOptions.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Addrs: []string{"127.0.0.2"}}, nil
	}
	secondOptions.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		secondReady <- struct{}{}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- run(ctx, config, secondOptions) }()
	select {
	case <-secondReady:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("restarted daemon did not reach readiness")
	}
	connection, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.2:%d", controlPort), time.Second)
	if err != nil {
		t.Fatalf("restarted control endpoint was not bound: %v", err)
	}
	_ = connection.Close()
	cancel()
	if err := waitRuntime(t, secondDone); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("daemon address restart mutated detached worker metadata")
	}
}
