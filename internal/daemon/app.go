package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/identity"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/worker"
)

const (
	defaultReconcileInterval = time.Second
	defaultWebSocketPath     = "/mesh"
	databaseName             = "mesh.db"
	sessionsDirectoryName    = "s"
)

// Config identifies the state and optional Tailnet listener owned by a daemon.
// A zero TailnetPort serves local Unix-socket clients only.
type Config struct {
	StateDir      string
	TailnetPort   uint16
	WebSocketPath string
	ReportError   func(error)
}

type runOptions struct {
	now               func() time.Time
	bootID            func() string
	discoverSelf      func(context.Context) (tailnet.Peer, error)
	reconcileInterval time.Duration
}

func defaultRunOptions() runOptions {
	return runOptions{
		now:               time.Now,
		bootID:            worker.BootID,
		discoverSelf:      tailnet.Self,
		reconcileInterval: defaultReconcileInterval,
	}
}

// SocketPath returns the local protocol socket owned by a daemon in stateDir.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, daemonSocketName)
}

// Run discovers local workers and serves clients until ctx is cancelled. It
// never waits for, signals, or otherwise owns a worker process.
func Run(ctx context.Context, cfg Config) error {
	return run(ctx, cfg, defaultRunOptions())
}

func run(ctx context.Context, cfg Config, opts runOptions) (runErr error) {
	if ctx == nil {
		return errors.New("daemon: nil context")
	}
	if ctx.Err() != nil {
		return nil
	}
	if cfg.StateDir == "" {
		return errors.New("daemon: state directory is empty")
	}
	if opts.now == nil || opts.bootID == nil || opts.discoverSelf == nil {
		return errors.New("daemon: incomplete runtime dependencies")
	}
	if opts.reconcileInterval <= 0 {
		return errors.New("daemon: reconciliation interval must be positive")
	}
	if cfg.WebSocketPath == "" {
		cfg.WebSocketPath = defaultWebSocketPath
	}
	if err := validateWebSocketPath(cfg.WebSocketPath); err != nil {
		return err
	}

	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("daemon: resolve state directory %s: %w", cfg.StateDir, err)
	}
	sessionsDir := filepath.Join(stateDir, sessionsDirectoryName)
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		return fmt.Errorf("daemon: create sessions directory %s: %w", sessionsDir, err)
	}
	lock, err := acquireDaemonLock(filepath.Join(stateDir, daemonLockName))
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, lock.release())
	}()
	daemonCtx, cancelDaemon := context.WithCancel(ctx)
	defer cancelDaemon()

	meshHost, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return fmt.Errorf("daemon: load host identity: %w", err)
	}
	reporter := newErrorReporter(cfg.ReportError)
	var tailnetAddrs []string
	var tailscaleName *string
	if cfg.TailnetPort != 0 {
		peer, discoverErr := opts.discoverSelf(daemonCtx)
		if discoverErr != nil {
			if daemonCtx.Err() != nil {
				return nil
			}
			reporter.report(fmt.Errorf("daemon: Tailnet listener disabled: %w", discoverErr))
		} else {
			tailnetAddrs = append([]string(nil), peer.Addrs...)
			if peer.Name != "" {
				name := peer.Name
				tailscaleName = &name
			}
			if len(tailnetAddrs) == 0 {
				reporter.report(errors.New("daemon: Tailnet listener disabled: this host has no Tailscale addresses"))
			}
		}
	}

	now := opts.now().UTC()
	host := storage.Host{
		ID:            storage.HostID(meshHost.ID),
		MeshIdentity:  meshHost.ID,
		TailscaleName: tailscaleName,
		LastSeenAt:    now,
	}
	store, err := storage.Open(daemonCtx, filepath.Join(stateDir, databaseName))
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, store.Close())
	}()
	services, err := store.ListServices(daemonCtx)
	if err != nil {
		return fmt.Errorf("daemon: restore services: %w", err)
	}
	serviceRegistry, err := meshserve.NewRegistryWithReservedPrefix(services, cfg.WebSocketPath)
	if err != nil {
		return fmt.Errorf("daemon: restore services: %w", err)
	}
	serviceControl, err := newServiceController(store, serviceRegistry)
	if err != nil {
		return err
	}

	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: sessionsDir,
		Host:        host,
		Store:       store,
		Probe:       newUnixWorkerProbe(),
		BootID:      opts.bootID,
		Now:         opts.now,
	})
	if err != nil {
		return err
	}
	if err := catalog.Reconcile(daemonCtx); err != nil {
		return err
	}
	connector, err := newWorkerConnector(sessionsDir, catalog)
	if err != nil {
		return err
	}
	lifecycle, err := newLifecycle(lifecycleConfig{
		Context:     daemonCtx,
		Catalog:     catalog,
		Connector:   connector,
		Host:        host,
		SessionsDir: sessionsDir,
	})
	if err != nil {
		return err
	}
	server, err := newClientServer(lifecycle, connector, serviceControl)
	if err != nil {
		return err
	}

	listener, err := validateListenerConfig(daemonCtx, ListenerConfig{
		StateDir:      stateDir,
		TailnetAddrs:  tailnetAddrs,
		TailnetPort:   cfg.TailnetPort,
		WebSocketPath: cfg.WebSocketPath,
		HTTPHandler:   serviceRegistry,
		ReportError:   reporter.report,
	}, server.Handle)
	if err != nil {
		return err
	}

	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		reconcilePeriodically(daemonCtx, catalog, opts.reconcileInterval, reporter)
	}()
	serveErr := serveListeners(daemonCtx, cancelDaemon, listener, server.Handle)
	cancelDaemon()
	<-reconciled
	return serveErr
}

func reconcilePeriodically(ctx context.Context, catalog *Catalog, interval time.Duration, reporter *errorReporter) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := catalog.Reconcile(ctx); err != nil && ctx.Err() == nil {
				reporter.report(fmt.Errorf("daemon: periodic reconciliation: %w", err))
			}
		}
	}
}
