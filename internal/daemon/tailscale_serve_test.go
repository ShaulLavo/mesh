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

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
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

func TestVerifyTailscaleServeForwardBoundsTheCheckAndReportsRemediation(t *testing.T) {
	var gotPort uint16
	err := verifyTailscaleServeForward(context.Background(), 8443, time.Second, func(ctx context.Context, port uint16) error {
		gotPort = port
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			return errors.New("verification context is not bounded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPort != 8443 {
		t.Fatalf("verified port = %d, want 8443", gotPort)
	}

	cause := errors.New("TCP/443 is not configured")
	err = verifyTailscaleServeForward(context.Background(), 8443, time.Second, func(context.Context, uint16) error {
		return cause
	})
	wantCommand := "tailscale serve --bg --yes --tcp=443 tcp://127.0.0.1:8443"
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), wantCommand) {
		t.Fatalf("verification failure = %v, want cause and %q", err, wantCommand)
	}
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
	signerID := installRunTestPrivateName(t, stateDir, httpsPort)
	configured := make(chan error, 1)
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Name: "origin.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.verifyServeForward = func(context.Context, uint16) error { return nil }
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
		if err == nil {
			probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			privateName, probeErr := probeWebSocketPrivateName(probeCtx, controlPort)
			cancel()
			if probeErr != nil {
				err = probeErr
			} else if privateName != "" {
				err = fmt.Errorf("private name %q was exposed before Tailscale Serve succeeded", privateName)
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
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		privateName, probeErr := probeWebSocketPrivateName(probeCtx, controlPort)
		probeCancel()
		if probeErr == nil && privateName == "pc.mesh.shaulavo.dev" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("private name was not exposed after Tailscale Serve readiness: name %q, error %v", privateName, probeErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRunFailsWhenTailscaleServeVerificationFails(t *testing.T) {
	stateDir := t.TempDir()
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	controlPort := reserveTCPPort(t, "127.0.0.1")
	signerID := installRunTestPrivateName(t, stateDir, httpsPort)
	verifyErr := errors.New("TCP/443 is not configured")
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Name: "origin.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	options.verifyServeForward = func(context.Context, uint16) error {
		probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		privateName, err := probeWebSocketPrivateName(probeCtx, controlPort)
		cancel()
		if err != nil {
			return err
		}
		if privateName != "" {
			return fmt.Errorf("private name %q was exposed before forwarding was verified", privateName)
		}
		return verifyErr
	}
	err := run(context.Background(), Config{
		StateDir: stateDir, TailnetPort: controlPort, HTTPSPort: httpsPort, CertificateRenewerID: signerID, TailscaleServe: true,
	}, options)
	wantCommand := fmt.Sprintf("tailscale serve --bg --yes --tcp=443 tcp://127.0.0.1:%d", httpsPort)
	if !errors.Is(err, verifyErr) || !strings.Contains(err.Error(), wantCommand) {
		t.Fatalf("daemon Tailscale Serve verification failure = %v", err)
	}
	if _, statErr := os.Lstat(SocketPath(stateDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon socket after Tailscale Serve verification failure: %v", statErr)
	}
}

func TestRunFailsWhenTailscaleServeConfigurationFails(t *testing.T) {
	stateDir := t.TempDir()
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	controlPort := reserveTCPPort(t, "127.0.0.1")
	signerID := installRunTestPrivateName(t, stateDir, httpsPort)
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Name: "origin.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		privateName, err := probeWebSocketPrivateName(probeCtx, controlPort)
		cancel()
		if err != nil {
			return nil, err
		}
		if privateName != "" {
			return nil, fmt.Errorf("private name %q was exposed before failed Tailscale Serve configuration", privateName)
		}
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

func TestRunAcceptsVerifiedOperatorManagedTailscaleServeForward(t *testing.T) {
	stateDir := t.TempDir()
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	signerID := installRunTestPrivateName(t, stateDir, httpsPort)
	options := defaultRunOptions()
	options.reconcileInterval = time.Hour
	var verified atomic.Bool
	options.verifyServeForward = func(_ context.Context, port uint16) error {
		verified.Store(true)
		if port != httpsPort {
			return fmt.Errorf("verified port %d, want %d", port, httpsPort)
		}
		return nil
	}
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("configuration command must not run for an operator-managed route")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{StateDir: stateDir, HTTPSPort: httpsPort, CertificateRenewerID: signerID}, options)
	}()
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		privateName, err := probeUnixPrivateName(probeCtx, SocketPath(stateDir))
		probeCancel()
		if err == nil {
			if privateName != "pc.mesh.shaulavo.dev" {
				t.Fatalf("private name with verified operator route = %q", privateName)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe daemon without Tailscale Serve: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !verified.Load() {
		t.Fatal("operator-managed Tailscale Serve route was not verified")
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
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
	defer blocked.Close()                                     //nolint:errcheck // test cleanup
	controlPort := uint16(blocked.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16
	signerID, _ := composedIdentity(t)
	called := false
	options := defaultRunOptions()
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Addrs: []string{"127.0.0.1", "127.0.0.2"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.verifyServeForward = func(context.Context, uint16) error { return nil }
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
	options.verifyServeForward = func(context.Context, uint16) error { return nil }
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
	before, err := os.ReadFile(metaPath) //nolint:gosec // test reads its own temporary worker metadata fixture
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
	firstOptions.verifyServeForward = func(context.Context, uint16) error { return nil }
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
	secondOptions.verifyServeForward = func(context.Context, uint16) error { return nil }
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
	after, err := os.ReadFile(metaPath) //nolint:gosec // test reads its own temporary worker metadata fixture
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("daemon address restart mutated detached worker metadata")
	}
}

func installRunTestPrivateName(t *testing.T, stateDir string, httpsPort uint16) string {
	t.Helper()
	target, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	signerID, signer := composedIdentity(t)
	runtime, err := configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: target.ID, OriginHTTPSPort: httpsPort, OriginRenewerID: signerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller, ok := runtime.Controller.(*certificateController)
	if !ok {
		t.Fatalf("certificate controller = %T", runtime.Controller)
	}
	now := time.Now().UTC()
	certificate, privateKey := daemonTestCertificate(t, 991, now)
	bundle, err := dnsname.ValidateBundle(certificate, privateKey, dnsname.WildcardName, now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := dnsname.SignBundle(bundle, target.ID, dnsname.ProfilePrivateOrigin, dnsname.EnvironmentLive, "pc.mesh.shaulavo.dev", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.installers[dnsname.ProfilePrivateOrigin].Install(signed); err != nil {
		t.Fatal(err)
	}
	return signerID
}

func probeWebSocketPrivateName(ctx context.Context, port uint16) (string, error) {
	conn, err := transport.DialOnce(ctx, fmt.Sprintf("ws://127.0.0.1:%d/mesh", port), transport.DialOptions{})
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint:errcheck // probe result is authoritative
	return probePrivateName(ctx, conn)
}

func probeUnixPrivateName(ctx context.Context, socketPath string) (string, error) {
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return "", err
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return "", err
	}
	defer conn.Close() //nolint:errcheck // probe result is authoritative
	return probePrivateName(ctx, conn)
}

func probePrivateName(ctx context.Context, conn transport.Conn) (string, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	request := protocol.Control{Type: protocol.TypeHostInfo, RequestID: "private-name-probe"}
	payload, err := request.Encode()
	if err != nil {
		return "", err
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return "", err
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return "", err
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return "", err
	}
	if response.Type != protocol.TypeHostInfoResult || response.RequestID != request.RequestID || response.Host == nil {
		return "", errors.New("invalid host-info probe response")
	}
	return response.Host.PrivateName, nil
}
