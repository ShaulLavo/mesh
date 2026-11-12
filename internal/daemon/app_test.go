package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/worker"
)

func TestRunRestoresPersistedServicesOnRestart(t *testing.T) {
	fixture := newDaemonRunFixture(t)
	stateDir := fixture.stateDir
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("survived restart"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), filepath.Join(stateDir, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertService(context.Background(), meshserve.Service{Name: "site", Kind: meshserve.Static, Target: root}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	options := runOptions{
		now:    func() time.Time { return catalogTestTime },
		bootID: func() string { return "boot-a" },
		discoverSelf: func(context.Context) (tailnet.Peer, error) {
			return tailnet.Peer{Name: "pc.example.ts.net", Addrs: []string{"127.0.0.1"}}, nil
		},
		discoverPeers:     func(context.Context) ([]tailnet.Peer, error) { return nil, nil },
		reconcileInterval: time.Hour,
	}
	for restart := 0; restart < 2; restart++ {
		listener, port := newTCPListener(t, "127.0.0.1:0")
		url := fmt.Sprintf("http://127.0.0.1:%d/site/", port)
		options.listen = useTCPListeners(listener)
		fixture.start(t, func(ctx context.Context) error {
			return run(ctx, Config{StateDir: stateDir, TailnetPort: port}, options)
		})
		body := waitForHTTPBody(t, url, http.StatusOK)
		if body != "survived restart" {
			t.Fatalf("restart %d body = %q", restart, body)
		}
		for _, host := range []string{"pc.example.ts.net", "pc", "rebind.attacker.example:7337"} {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = host
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := http.StatusOK
			if strings.HasPrefix(host, "rebind.") {
				want = http.StatusMisdirectedRequest
			}
			if response.StatusCode != want {
				t.Fatalf("restart %d Host %q returned %d, want %d", restart, host, response.StatusCode, want)
			}
			if want == http.StatusOK && string(responseBody) != "survived restart" {
				t.Fatalf("restart %d Host %q body = %q", restart, host, responseBody)
			}
		}
		fixture.stop(t)
	}
}

func waitForHTTPBody(t *testing.T, url string, wantStatus int) string {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == wantStatus {
				return string(body)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not return %d: %v", url, wantStatus, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunServesLocalClientsWhenTailnetDiscoveryFails(t *testing.T) {
	fixture := newDaemonRunFixture(t)
	stateDir := fixture.stateDir
	reported := make(chan error, 1)
	discoveryErr := errors.New("tailscale unavailable")
	fixture.start(t, func(ctx context.Context) error {
		return run(ctx, Config{
			StateDir:    stateDir,
			TailnetPort: 9443,
			ReportError: func(err error) { reported <- err },
		}, runOptions{
			now:               func() time.Time { return catalogTestTime },
			bootID:            func() string { return "boot-a" },
			discoverSelf:      func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, discoveryErr },
			reconcileInterval: time.Hour,
		})
	})

	client := dialUnixRuntime(t, SocketPath(stateDir))
	defer client.Close() //nolint:errcheck // test cleanup
	if err := client.WriteFrame(controlFrame(t, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "host-1"})); err != nil {
		t.Fatal(err)
	}
	frame, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != protocol.TypeHostInfoResult || response.RequestID != "host-1" || response.Host == nil {
		t.Fatalf("host response = %#v", response)
	}
	identityHost, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if response.Host.ID != identityHost.ID || response.Host.MeshIdentity != identityHost.ID {
		t.Fatalf("host response = %#v, identity = %#v", response.Host, identityHost)
	}
	select {
	case got := <-reported:
		if !errors.Is(got, discoveryErr) {
			t.Fatalf("reported error = %v, want discovery failure", got)
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("Tailnet discovery failure was not reported")
	}

	fixture.stop(t)
	if _, err := os.Stat(filepath.Join(stateDir, databaseName)); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
}

func TestRunPublishesBootInterruptedSession(t *testing.T) {
	fixture := newDaemonRunFixture(t)
	stateDir := fixture.stateDir
	sessionsDir := filepath.Join(stateDir, sessionsDirectoryName)
	dir := filepath.Join(sessionsDir, "7K3D")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(dir, worker.Meta{
		ID:        "7K3D",
		PID:       os.Getpid(),
		Command:   []string{"sh", "-c", "sleep 60"},
		Cwd:       stateDir,
		State:     worker.StateRunning,
		CreatedAt: catalogTestTime,
		BootID:    "boot-before-restart",
	}); err != nil {
		t.Fatal(err)
	}

	fixture.start(t, func(ctx context.Context) error {
		return run(ctx, Config{StateDir: stateDir}, runOptions{
			now:               func() time.Time { return catalogTestTime.Add(time.Minute) },
			bootID:            func() string { return "boot-after-restart" },
			discoverSelf:      func(context.Context) (tailnet.Peer, error) { panic("unexpected Tailnet discovery") },
			reconcileInterval: time.Hour,
		})
	})

	client := fixture.dial(t)
	defer client.Close() //nolint:errcheck // test cleanup
	if err := client.WriteFrame(controlFrame(t, protocol.Control{Type: protocol.TypeList, RequestID: "list-1"})); err != nil {
		t.Fatal(err)
	}
	frame, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != protocol.TypeListed || response.RequestID != "list-1" || len(response.Sessions) != 1 {
		t.Fatalf("list response = %#v", response)
	}
	if got := response.Sessions[0]; got.ID != "7K3D" || got.State != string(storage.StateInterrupted) || !strings.Contains(strings.Join(got.Command, " "), "sleep") {
		t.Fatalf("listed session = %#v", got)
	}
}

func TestRunLosingDaemonCannotMutateCatalog(t *testing.T) {
	fixture := newDaemonRunFixture(t)
	stateDir := fixture.stateDir
	firstOptions := runOptions{
		now:               func() time.Time { return catalogTestTime },
		bootID:            func() string { return "boot-a" },
		discoverSelf:      func(context.Context) (tailnet.Peer, error) { panic("unexpected Tailnet discovery") },
		reconcileInterval: time.Hour,
	}
	fixture.start(t, func(ctx context.Context) error {
		return run(ctx, Config{StateDir: stateDir}, firstOptions)
	})
	client := fixture.dial(t)
	_ = client.Close()

	loserOptions := firstOptions
	loserOptions.now = func() time.Time { return catalogTestTime.Add(24 * time.Hour) }
	if err := run(context.Background(), Config{StateDir: stateDir}, loserOptions); !errors.Is(err, ErrDaemonAlreadyRunning) {
		t.Fatalf("second Run error = %v, want ErrDaemonAlreadyRunning", err)
	}

	meshHost, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), filepath.Join(stateDir, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	host, err := store.GetHost(context.Background(), storage.HostID(meshHost.ID))
	closeErr := store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !host.LastSeenAt.Equal(catalogTestTime) {
		t.Fatalf("last seen time = %v, losing daemon wrote %v", host.LastSeenAt, loserOptions.now())
	}
}

func TestRunShutdownDoesNotWaitForBlockedErrorSink(t *testing.T) {
	reportStarted := make(chan struct{})
	startReport := sync.OnceFunc(func() { close(reportStarted) })
	releaseReport := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseReport) })
	defer release()
	reportError := func(error) {
		startReport()
		<-releaseReport
	}
	fixture := newDaemonRunFixture(t)
	stateDir := fixture.stateDir
	fixture.start(t, func(ctx context.Context) error {
		return run(ctx, Config{
			StateDir: stateDir, TailnetPort: 7337,
			ReportError: reportError,
		}, runOptions{
			now: func() time.Time { return catalogTestTime }, bootID: func() string { return "boot" },
			discoverSelf: func(context.Context) (tailnet.Peer, error) {
				return tailnet.Peer{}, errors.New("tailscale unavailable")
			},
			reconcileInterval: time.Hour,
		})
	})
	client := dialUnixRuntime(t, SocketPath(stateDir))
	_ = client.Close()
	select {
	case <-reportStarted:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("error callback did not start")
	}
	fixture.stop(t)

	// Queued notifications can reach the sink after shutdown releases it.
	release()
	reportError(errors.New("additional notification"))
}
