package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/worker"
)

const (
	defaultReconcileInterval          = time.Second
	defaultTailnetAddressPollInterval = 30 * time.Second
	defaultTailnetDiscoveryTimeout    = 15 * time.Second
	defaultWebSocketPath              = "/mesh"
	databaseName                      = "mesh.db"
	sessionsDirectoryName             = "s"
)

// Config identifies the state and optional Tailnet listener owned by a daemon.
// A zero TailnetPort serves local Unix-socket clients only.
type Config struct {
	StateDir             string
	TailnetPort          uint16
	WebSocketPath        string
	HTTPSPort            uint16
	CertificateRenewerID string
	PrivateNamesConfig   string
	TailscaleServe       bool
	ReportError          func(error)
}

type runOptions struct {
	now                     func() time.Time
	bootID                  func() string
	discoverSelf            func(context.Context) (tailnet.Peer, error)
	validateServeAddresses  func([]string) error
	reconcileInterval       time.Duration
	tailnetPollInterval     time.Duration
	tailnetDiscoveryTimeout time.Duration
	runCommand              externalCommand
	tailscaleTimeout        time.Duration
}

func defaultRunOptions() runOptions {
	return runOptions{
		now:                     time.Now,
		bootID:                  worker.BootID,
		discoverSelf:            tailnet.Self,
		validateServeAddresses:  validateTailscaleServeAddresses,
		reconcileInterval:       defaultReconcileInterval,
		tailnetPollInterval:     defaultTailnetAddressPollInterval,
		tailnetDiscoveryTimeout: defaultTailnetDiscoveryTimeout,
		runCommand:              runExternalCommand,
		tailscaleTimeout:        defaultTailscaleServeTimeout,
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
	if cfg.TailscaleServe {
		if cfg.HTTPSPort == 0 {
			return errors.New("daemon: Tailscale Serve requires a non-zero HTTPS port")
		}
		if cfg.TailnetPort == 0 {
			return errors.New("daemon: Tailscale Serve requires a non-zero Tailnet control port")
		}
		if cfg.TailnetPort == 443 {
			return errors.New("daemon: Tailscale Serve TCP/443 conflicts with the direct Tailnet listener on port 443")
		}
		if opts.runCommand == nil || opts.tailscaleTimeout <= 0 || opts.validateServeAddresses == nil || opts.tailnetPollInterval <= 0 || opts.tailnetDiscoveryTimeout <= 0 {
			return errors.New("daemon: incomplete Tailscale Serve runtime dependencies")
		}
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

	meshHost, meshPrivateKey, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return fmt.Errorf("daemon: load host identity: %w", err)
	}
	reporter := newErrorReporter(cfg.ReportError)
	var tailnetAddrs []string
	var tailscaleName *string
	if cfg.TailnetPort != 0 {
		discoveryCtx := daemonCtx
		cancelDiscovery := func() {}
		if cfg.TailscaleServe {
			discoveryCtx, cancelDiscovery = context.WithTimeout(daemonCtx, opts.tailnetDiscoveryTimeout)
		}
		peer, discoverErr := opts.discoverSelf(discoveryCtx)
		cancelDiscovery()
		if discoverErr != nil {
			if daemonCtx.Err() != nil {
				return nil
			}
			if cfg.TailscaleServe {
				return fmt.Errorf("daemon: discover Tailscale addresses required by Tailscale Serve: %w", discoverErr)
			}
			reporter.report(fmt.Errorf("daemon: Tailnet listener disabled: %w", discoverErr))
		} else {
			tailnetAddrs, err = normalizeTailnetAddresses(peer.Addrs)
			if err != nil {
				return fmt.Errorf("daemon: normalize discovered Tailscale addresses: %w", err)
			}
			if peer.Name != "" {
				name := peer.Name
				tailscaleName = &name
			}
			if len(tailnetAddrs) == 0 {
				if cfg.TailscaleServe {
					return errors.New("daemon: Tailscale Serve requires at least one discovered Tailscale address")
				}
				reporter.report(errors.New("daemon: Tailnet listener disabled: this host has no Tailscale addresses"))
			}
			if cfg.TailscaleServe {
				if err := opts.validateServeAddresses(tailnetAddrs); err != nil {
					return err
				}
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
	certificateControl, tlsConfig, err := configureOriginCertificates(stateDir, meshHost.ID, cfg.CertificateRenewerID, cfg.HTTPSPort)
	if err != nil {
		return err
	}
	var privateNamesRuntime *dnsname.PrivateNamesRuntime
	if cfg.PrivateNamesConfig != "" {
		privateNamesRuntime, err = dnsname.NewPrivateNamesRuntime(cfg.PrivateNamesConfig, dnsname.PrivateNamesRuntimeOptions{
			StateDir: stateDir, Signer: meshPrivateKey, Distribute: true,
		})
		if err != nil {
			return fmt.Errorf("daemon: configure private names: %w", err)
		}
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
	server, err := newClientServer(lifecycle, connector, serviceControl, certificateControl)
	if err != nil {
		return err
	}

	listener, err := validateListenerConfig(daemonCtx, ListenerConfig{
		StateDir:                   stateDir,
		TailnetAddrs:               tailnetAddrs,
		TailnetPort:                cfg.TailnetPort,
		WebSocketPath:              cfg.WebSocketPath,
		HTTPHandler:                serviceRegistry,
		HTTPSPort:                  cfg.HTTPSPort,
		TLSConfig:                  tlsConfig,
		RequireAllTailnetListeners: cfg.TailscaleServe,
		ReportError:                reporter.report,
	}, server.Handle)
	if err != nil {
		return err
	}
	listenersReady := make(chan struct{})
	listener.ready = func(readyCtx context.Context) error {
		if cfg.TailscaleServe {
			if err := configureTailscaleServe(readyCtx, cfg.HTTPSPort, opts.tailscaleTimeout, opts.runCommand); err != nil {
				return err
			}
		}
		close(listenersReady)
		return nil
	}

	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		reconcilePeriodically(daemonCtx, catalog, opts.reconcileInterval, reporter)
	}()
	privateNamesDone := make(chan struct{})
	go func() {
		defer close(privateNamesDone)
		if privateNamesRuntime == nil {
			return
		}
		select {
		case <-listenersReady:
		case <-daemonCtx.Done():
			return
		}
		if err := privateNamesRuntime.Manager.Run(daemonCtx, privateNamesRuntime.Interval, func(err error) {
			reporter.report(fmt.Errorf("daemon: reconcile private names: %w", err))
		}); err != nil && daemonCtx.Err() == nil {
			reporter.report(fmt.Errorf("daemon: private-names loop: %w", err))
		}
	}()
	tailnetMonitorDone := make(chan error, 1)
	go func() {
		if !cfg.TailscaleServe {
			tailnetMonitorDone <- nil
			return
		}
		select {
		case <-listenersReady:
		case <-daemonCtx.Done():
			tailnetMonitorDone <- nil
			return
		}
		monitorErr := monitorTailnetAddresses(
			daemonCtx,
			opts.tailnetPollInterval,
			opts.tailnetDiscoveryTimeout,
			tailnetAddrs,
			opts.discoverSelf,
			opts.validateServeAddresses,
			reporter.report,
		)
		if monitorErr != nil {
			cancelDaemon()
		}
		tailnetMonitorDone <- monitorErr
	}()
	serveErr := serveListeners(daemonCtx, cancelDaemon, listener, server.Handle)
	cancelDaemon()
	<-reconciled
	<-privateNamesDone
	return errors.Join(serveErr, <-tailnetMonitorDone)
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
