package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestCatalogOpenContentionKeepsAnOptionalCache(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", compactSocketTempDir(t))
	release := holdCatalogWriter(t, os.Getenv("MESH_STATE_DIR"))
	type openedCatalog struct {
		cache *SQLiteCatalogCache
		err   error
	}
	done := make(chan openedCatalog, 1)
	finished := make(chan struct{})
	var opened *SQLiteCatalogCache
	t.Cleanup(func() {
		release()
		<-finished
		if opened != nil {
			if err := opened.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	go func() {
		defer close(finished)
		cache, err := OpenCatalogCache(context.Background())
		opened = cache
		done <- openedCatalog{cache: cache, err: err}
	}()
	var result openedCatalog
	select {
	case result = <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("opening the optional cache waited on another SQLite writer")
	}
	if result.err != nil || result.cache == nil {
		t.Fatalf("cache open must remain nonfatal during contention: %#v", result)
	}
	host := HostRecord{ID: "host-pc", Alias: "pc"}
	rows, err := result.cache.Load(t.Context(), host)
	var busy *sqlite.Error
	if len(rows) != 0 || !errors.As(err, &busy) || busy.Code()&0xff != 5 {
		t.Fatalf("missing-cache warning = %#v, %v, want SQLITE_BUSY", rows, err)
	}
	if err := result.cache.Save(t.Context(), host, nil); err != nil {
		t.Fatalf("cache open warning repeated on save: %v", err)
	}
	services, err := result.cache.LoadAllServices(t.Context())
	if err != nil || len(services) != 0 {
		t.Fatalf("optional service cache blocks live fan-out: %#v, %v", services, err)
	}
	servicesForHost, err := result.cache.LoadServices(t.Context(), host)
	if err != nil || len(servicesForHost) != 0 {
		t.Fatalf("cache open warning repeated on service read: %#v, %v", servicesForHost, err)
	}
	if err := result.cache.SaveServices(t.Context(), host, "", nil); err != nil {
		t.Fatalf("cache open warning repeated on service save: %v", err)
	}
}

func TestCatalogOpenContentionKeepsServiceFanoutLive(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", compactSocketTempDir(t))
	release := holdCatalogWriter(t, os.Getenv("MESH_STATE_DIR"))
	defer release()
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	hosts := []HostRecord{{ID: "host-pc", Alias: "pc"}, {ID: "host-pi", Alias: "pi"}}
	rows, diagnostics, err := CollectServiceCatalog(t.Context(), hosts, 40*time.Millisecond,
		func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
			return remoteServiceSnapshot{Services: []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", Healthy: true}}}, nil
		}, nil, cache)
	if err != nil || len(rows) != len(hosts) {
		t.Fatalf("live service fan-out = %#v, %v", rows, err)
	}
	for _, row := range rows {
		if !row.Live || row.Stale || row.Service.Name != "api" {
			t.Fatalf("optional cache replaced live service authority: %#v", row)
		}
	}
	if len(diagnostics) != 1 || len(unavailableServiceAliases(diagnostics)) != 0 {
		t.Fatalf("service diagnostics = %#v, want one nonfatal cache warning", diagnostics)
	}
	for _, diagnostic := range diagnostics {
		var busy *sqlite.Error
		if !errors.As(diagnostic, &busy) || busy.Code()&0xff != 5 {
			t.Fatalf("cache warning = %v, want SQLITE_BUSY", diagnostic)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.Load(ctx, hosts[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("disabled cache swallowed cancellation: %v", err)
	}
}

func TestConcurrentListCommandsSurviveCatalogOpenContention(t *testing.T) {
	host := setupCommandTestHost(t)
	other := &commandTestHost{host: host.host}
	other.host.Alias = "pi"
	other.host.ID = "host-pi"
	other.host.MeshIdentity = "identity-pi"
	if err := SaveHost(other.host); err != nil {
		t.Fatal(err)
	}
	if _, _, err := identity.LoadOrCreate(os.Getenv("MESH_STATE_DIR")); err != nil {
		t.Fatal(err)
	}
	release := holdCatalogWriter(t, os.Getenv("MESH_STATE_DIR"))
	type commandResult struct {
		stdout string
		stderr string
		err    error
	}
	done := make(chan commandResult, 2)
	var commands sync.WaitGroup
	t.Cleanup(func() { release(); commands.Wait() })
	for range 2 {
		commands.Add(1)
		go func() {
			defer commands.Done()
			stdout, stderr, err := executeCommand(t, Dependencies{
				DialHost: func(ctx context.Context, queried HostRecord) (transport.Conn, error) {
					if queried.ID == host.host.ID {
						return host.dial(ctx, queried)
					}
					return other.dial(ctx, queried)
				},
				Now: func() time.Time { return commandTestTime },
			}, "ls", "--timeout", "40ms")
			done <- commandResult{stdout: stdout, stderr: stderr, err: err}
		}()
	}
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	for range 2 {
		select {
		case result := <-done:
			if result.err != nil || strings.Count(result.stdout, "7K3D") != 2 {
				t.Fatalf("live list failed during cache contention: %+v", result)
			}
			if warnings := strings.Count(result.stderr, "live results could not be cached"); warnings != 1 {
				t.Fatalf("cache warnings = %d, want one per command: %q", warnings, result.stderr)
			}
		case <-timer.C:
			t.Fatal("concurrent list commands waited on the optional cache writer lock")
		}
	}
}
