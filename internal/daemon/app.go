package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/inhibit"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/sshd"
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

// Config identifies the state and optional listeners owned by a daemon. Zero
// TailnetPort and SSHPort values disable their corresponding listeners.
type Config struct {
	SSHSessionHandler    sshd.SessionHandlerFactory
	StateDir             string
	TailnetPort          uint16
	SSHPort              uint16
	WebSocketPath        string
	HTTPSPort            uint16
	CertificateRenewerID string
	PrivateNamesConfig   string
	EdgeConfig           string
	PublicEdgeTarget     string
	TailscaleServe       bool
	ReportError          func(error)
}

type runOptions struct {
	now                     func() time.Time
	bootID                  func() string
	discoverSelf            func(context.Context) (tailnet.Peer, error)
	discoverPeers           func(context.Context) ([]tailnet.Peer, error)
	validateServeAddresses  func([]string) error
	reconcileInterval       time.Duration
	tailnetPollInterval     time.Duration
	tailnetDiscoveryTimeout time.Duration
	runCommand              externalCommand
	verifyServeForward      serveForwardVerifier
	tailscaleTimeout        time.Duration
	serveSSH                func(context.Context, sshd.Config) error
}

func defaultRunOptions() runOptions {
	return runOptions{
		now:                     time.Now,
		bootID:                  worker.BootID,
		discoverSelf:            tailnet.Self,
		discoverPeers:           tailnet.Peers,
		validateServeAddresses:  validateTailscaleServeAddresses,
		reconcileInterval:       defaultReconcileInterval,
		tailnetPollInterval:     defaultTailnetAddressPollInterval,
		tailnetDiscoveryTimeout: defaultTailnetDiscoveryTimeout,
		runCommand:              runExternalCommand,
		verifyServeForward:      tailnet.VerifyServeForward,
		tailscaleTimeout:        defaultTailscaleServeTimeout,
		serveSSH:                func(ctx context.Context, cfg sshd.Config) error { return sshd.Serve(ctx, cfg) },
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
	if cfg.HTTPSPort != 0 && (opts.verifyServeForward == nil || opts.tailscaleTimeout <= 0) {
		return errors.New("daemon: incomplete private HTTPS forwarding dependencies")
	}
	if cfg.SSHPort != 0 && opts.serveSSH == nil {
		return errors.New("daemon: incomplete SSH runtime dependencies")
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
		if opts.runCommand == nil || opts.validateServeAddresses == nil || opts.tailnetPollInterval <= 0 || opts.tailnetDiscoveryTimeout <= 0 {
			return errors.New("daemon: incomplete Tailscale Serve runtime dependencies")
		}
	}
	if cfg.WebSocketPath == "" {
		cfg.WebSocketPath = defaultWebSocketPath
	}
	if err := validateWebSocketPath(cfg.WebSocketPath); err != nil {
		return err
	}
	var publicEdgeConfig *edge.RuntimeConfig
	if cfg.EdgeConfig != "" {
		loaded, err := edge.LoadRuntimeConfig(cfg.EdgeConfig)
		if err != nil {
			return fmt.Errorf("daemon: configure public edge: %w", err)
		}
		publicEdgeConfig = &loaded
	}
	var publicEdgeTarget *edge.TargetConfig
	if cfg.PublicEdgeTarget != "" {
		loaded, err := edge.LoadTargetConfig(cfg.PublicEdgeTarget)
		if err != nil {
			return fmt.Errorf("daemon: configure public edge publisher: %w", err)
		}
		publicEdgeTarget = &loaded
	}
	if publicEdgeConfig != nil && cfg.PrivateNamesConfig != "" {
		return errors.New("daemon: public edge mode cannot load the Pi-only private-names configuration")
	}
	requiresStableTailnetControl := cfg.TailscaleServe || publicEdgeConfig != nil || publicEdgeTarget != nil
	if (publicEdgeConfig != nil || publicEdgeTarget != nil) && opts.discoverPeers == nil {
		return errors.New("daemon: public edge roles require Tailscale peer discovery")
	}
	if requiresStableTailnetControl && (opts.tailnetPollInterval <= 0 || opts.tailnetDiscoveryTimeout <= 0) {
		return errors.New("daemon: stable Tailnet control requires positive discovery and monitor timeouts")
	}
	if (publicEdgeConfig != nil || publicEdgeTarget != nil) && cfg.TailnetPort == 0 {
		return errors.New("daemon: public edge roles require a non-zero Tailnet control port")
	}

	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("daemon: resolve state directory %s: %w", cfg.StateDir, err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("daemon: resolve service home: %w", err)
	}
	homeDir, err = filepath.Abs(homeDir)
	if err != nil {
		return fmt.Errorf("daemon: resolve service home %s: %w", homeDir, err)
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
	defer reporter.shutdown()
	power, err := newWakeController(daemonCtx, stateDir, meshPrivateKey, opts.discoverPeers)
	if err != nil {
		return fmt.Errorf("daemon: configure waking: %w", err)
	}
	discoverAllPeers := func(discoveryCtx context.Context) ([]tailnet.Peer, error) {
		self, selfErr := opts.discoverSelf(discoveryCtx)
		peers, peersErr := opts.discoverPeers(discoveryCtx)
		if selfErr != nil && peersErr != nil {
			return nil, errors.Join(selfErr, peersErr)
		}
		if selfErr != nil {
			reporter.report(fmt.Errorf("daemon: discover local Tailscale peer for public edge: %w", selfErr))
			return peers, nil
		}
		if peersErr != nil {
			reporter.report(fmt.Errorf("daemon: discover remote Tailscale peers for public edge: %w", peersErr))
			return []tailnet.Peer{self}, nil
		}
		return append([]tailnet.Peer{self}, peers...), nil
	}
	var tailnetAddrs []string
	var tailscaleName *string
	if cfg.TailnetPort != 0 || cfg.SSHPort != 0 {
		disabledListeners := "Tailnet control listener"
		if cfg.SSHPort != 0 {
			disabledListeners = "SSH listener"
			if cfg.TailnetPort != 0 {
				disabledListeners = "Tailnet control and SSH listeners"
			}
		}
		// Bound discovery always, not only for the public-networking roles. A
		// wedged tailscale binary here blocks after the daemon lock is held and
		// before the Unix socket exists, so the host looks started, refuses a
		// second daemon, and answers nothing.
		discoveryCtx := daemonCtx
		cancelDiscovery := func() {}
		if opts.tailnetDiscoveryTimeout > 0 {
			discoveryCtx, cancelDiscovery = context.WithTimeout(daemonCtx, opts.tailnetDiscoveryTimeout)
		}
		peer, discoverErr := opts.discoverSelf(discoveryCtx)
		cancelDiscovery()
		if discoverErr != nil {
			if daemonCtx.Err() != nil {
				return nil
			}
			if requiresStableTailnetControl {
				return fmt.Errorf("daemon: discover Tailscale addresses required by configured public networking: %w", discoverErr)
			}
			reporter.report(fmt.Errorf("daemon: %s disabled: %w", disabledListeners, discoverErr))
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
				if requiresStableTailnetControl {
					return errors.New("daemon: configured public networking requires at least one discovered Tailscale address")
				}
				reporter.report(fmt.Errorf("daemon: %s disabled: this host has no Tailscale addresses", disabledListeners))
			}
			if cfg.TailscaleServe {
				if err := opts.validateServeAddresses(tailnetAddrs); err != nil {
					return err
				}
			} else if requiresStableTailnetControl {
				if err := validateTailscaleControlAddresses(tailnetAddrs); err != nil {
					return err
				}
			}
		}
	}
	sshAddrs := tailnetAddrs
	if cfg.SSHPort != 0 && len(sshAddrs) > 0 {
		if err := validateTailscaleControlAddresses(sshAddrs); err != nil {
			reporter.report(fmt.Errorf("daemon: SSH listener disabled: %w", err))
			sshAddrs = nil
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
	var pinnedPublicEdge atomic.Pointer[netip.Addr]
	var trustPublicEdgeForwarding func(netip.Addr) bool
	if publicEdgeTarget != nil {
		trustPublicEdgeForwarding = func(address netip.Addr) bool {
			pinned := pinnedPublicEdge.Load()
			return pinned != nil && *pinned == address.Unmap()
		}
	}
	serviceRegistry, err := meshserve.NewRegistryWithReservedPrefix(services, cfg.WebSocketPath, trustPublicEdgeForwarding)
	if err != nil {
		return fmt.Errorf("daemon: restore services: %w", err)
	}
	var edgeRegistry *edge.Registry
	var edgeControl controlHandler = disabledEdgeController{}
	var publicListenAddress string
	var publicHTTPHandler http.Handler
	var publicMode edge.Mode
	var publicCertificatePin string
	if publicEdgeConfig != nil {
		waker := &edgeWakeAdapter{client: power.client, origins: publicEdgeConfig.Origins, resolve: edge.TailscaleWakeResolver(discoverAllPeers)}
		edgeRegistry, err = edge.NewRegistry(edge.HandlerConfig{
			Mode: publicEdgeConfig.Mode, ReservedPath: cfg.WebSocketPath,
			Waker:  waker,
			Logger: log.New(edgeReportWriter{reporter: reporter}, "", 0),
		})
		if err != nil {
			return fmt.Errorf("daemon: configure public edge handler: %w", err)
		}
		defer edgeRegistry.Close()
		controller, err := edge.NewController(daemonCtx, edge.ControllerConfig{
			TargetID: meshHost.ID, Origins: publicEdgeConfig.Origins, State: store, Registry: edgeRegistry,
			Resolve: edge.TailscaleResolver(discoverAllPeers), Pin: waker.pin, Now: opts.now,
		})
		if err != nil {
			return fmt.Errorf("daemon: configure public edge registration: %w", err)
		}
		edgeControl = controller
		publicListenAddress = publicEdgeConfig.ListenAddress
		publicHTTPHandler = edgeRegistry
		publicMode = publicEdgeConfig.Mode
		publicCertificatePin = publicEdgeConfig.CertificateRenewerID
	}
	var publication servicePublisher = disabledServicePublisher{}
	if publicEdgeTarget != nil {
		publisher, err := edge.NewPublisher(edge.PublisherConfig{
			Signer: meshPrivateKey, Target: *publicEdgeTarget, State: store,
			Resolve: edge.TailscaleTargetResolver(discoverAllPeers), Now: opts.now, RequestTimeout: 5 * time.Second,
			OnPinned: func(address netip.Addr) {
				canonical := address.Unmap()
				pinnedPublicEdge.Store(&canonical)
			},
		})
		if err != nil {
			return fmt.Errorf("daemon: configure public edge publisher: %w", err)
		}
		publication = publisher
	}
	serviceControl, err := newServiceController(daemonCtx, homeDir, store, serviceRegistry, publication)
	if err != nil {
		return err
	}
	certificateRuntime, err := configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: meshHost.ID, OriginHTTPSPort: cfg.HTTPSPort,
		OriginRenewerID: cfg.CertificateRenewerID, PublicMode: publicMode, PublicCertificatePin: publicCertificatePin,
	})
	if err != nil {
		return err
	}
	var privateNamesRuntime *dnsname.PrivateNamesRuntime
	if cfg.PrivateNamesConfig != "" {
		privateNamesRuntime, err = dnsname.NewPrivateNamesRuntime(cfg.PrivateNamesConfig, dnsname.PrivateNamesRuntimeOptions{
			StateDir: stateDir, Signer: meshPrivateKey, Distribute: true,
			DiscoverSelf: opts.discoverSelf, DiscoverPeers: opts.discoverPeers,
		})
		if err != nil {
			return fmt.Errorf("daemon: configure private names: %w", err)
		}
	}

	inhibitor := inhibit.New(reporter.report)
	defer func() { runErr = errors.Join(runErr, inhibitor.Close()) }()
	catalog, err := NewCatalog(CatalogConfig{
		OnReconcile: syncSleepInhibitor(inhibitor.Update),
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
		PrivateName: certificateRuntime.PrivateName,
		SessionsDir: sessionsDir,
	})
	if err != nil {
		return err
	}
	server, err := newClientServer(lifecycle, connector, edgeControl, serviceControl, certificateRuntime.Controller)
	if err != nil {
		return err
	}
	server.wake = power

	controlAddrs := tailnetAddrs
	if cfg.TailnetPort == 0 {
		controlAddrs = nil
	}
	listener, err := validateListenerConfig(daemonCtx, ListenerConfig{
		StateDir:                   stateDir,
		TailnetAddrs:               controlAddrs,
		TailnetPort:                cfg.TailnetPort,
		WebSocketPath:              cfg.WebSocketPath,
		HTTPHandler:                serviceRegistry,
		HTTPSPort:                  cfg.HTTPSPort,
		TLSConfig:                  certificateRuntime.OriginTLS,
		PublicListenAddress:        publicListenAddress,
		PublicHTTPHandler:          publicHTTPHandler,
		PublicTLSConfig:            certificateRuntime.PublicTLS,
		RequireAllTailnetListeners: requiresStableTailnetControl,
		ReportError:                reporter.report,
	}, server.Handle)
	if err != nil {
		return err
	}
	if cfg.SSHPort != 0 {
		var sessionHandler sshd.SessionHandler
		if cfg.SSHSessionHandler != nil {
			sessionHandler = cfg.SSHSessionHandler(stateDir)
		}
		for _, address := range sshAddrs {
			listener.sshConfigs = append(listener.sshConfigs, sshd.Config{
				Handler:        sessionHandler,
				HostKey:        meshPrivateKey,
				AuthorizedKeys: filepath.Join(stateDir, "authorized_keys"),
				Addr:           net.JoinHostPort(address, strconv.Itoa(int(cfg.SSHPort))),
			})
		}
		listener.serveSSH = opts.serveSSH
	}
	listenersReady := make(chan struct{})
	listener.ready = func(readyCtx context.Context) error {
		if cfg.TailscaleServe {
			if err := configureTailscaleServe(readyCtx, cfg.HTTPSPort, opts.tailscaleTimeout, opts.runCommand); err != nil {
				return err
			}
		}
		if cfg.HTTPSPort != 0 {
			if err := verifyTailscaleServeForward(readyCtx, cfg.HTTPSPort, opts.tailscaleTimeout, opts.verifyServeForward); err != nil {
				return err
			}
			if certificateRuntime.PrivateNameReady != nil {
				certificateRuntime.PrivateNameReady()
			}
		}
		close(listenersReady)
		return nil
	}

	powerDone := make(chan struct{})
	go func() {
		defer close(powerDone)
		power.run(daemonCtx, listenersReady, cfg.TailnetPort != 0)
	}()
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
	publicCertificateDone := make(chan struct{})
	go func() {
		defer close(publicCertificateDone)
		if privateNamesRuntime == nil || privateNamesRuntime.PublicManager == nil {
			return
		}
		select {
		case <-listenersReady:
		case <-daemonCtx.Done():
			return
		}
		if err := privateNamesRuntime.PublicManager.Run(daemonCtx, privateNamesRuntime.Interval, func(err error) {
			reporter.report(fmt.Errorf("daemon: reconcile public certificate: %w", err))
		}); err != nil && daemonCtx.Err() == nil {
			reporter.report(fmt.Errorf("daemon: public-certificate loop: %w", err))
		}
	}()
	publicHeartbeatDone := make(chan struct{})
	go func() {
		defer close(publicHeartbeatDone)
		if !publication.Enabled() {
			return
		}
		select {
		case <-listenersReady:
		case <-daemonCtx.Done():
			return
		}
		serviceControl.RunPublicHeartbeat(daemonCtx, func(err error) {
			reporter.report(fmt.Errorf("daemon: reconcile public edge routes: %w", err))
		})
	}()
	tailnetMonitorDone := make(chan error, 1)
	// Watch Tailnet addresses whenever a listener depends on them, not only for
	// the public-networking roles. Startup discovery is one shot: if tailscaled
	// was not up yet, the tailnet control and SSH listeners were never created
	// for the life of the process, while the daemon kept serving the Unix
	// socket and so looked healthy to both systemd and launchd. The shipped
	// unit is a user unit, so its After=tailscaled.service cannot order against
	// a system unit and this is the ordinary boot race, not an edge case. The
	// monitor's existing address-changed path already restarts the daemon;
	// starting from the addresses we actually have makes it cover "none yet"
	// as well as "changed since".
	watchTailnetAddresses := requiresStableTailnetControl || cfg.TailnetPort != 0 || cfg.SSHPort != 0
	go func() {
		if !watchTailnetAddresses || opts.tailnetPollInterval <= 0 || opts.tailnetDiscoveryTimeout <= 0 {
			tailnetMonitorDone <- nil
			return
		}
		select {
		case <-listenersReady:
		case <-daemonCtx.Done():
			tailnetMonitorDone <- nil
			return
		}
		validateAddresses := validateTailscaleControlAddresses
		if cfg.TailscaleServe {
			validateAddresses = opts.validateServeAddresses
		}
		monitorErr := monitorTailnetAddresses(
			daemonCtx,
			opts.tailnetPollInterval,
			opts.tailnetDiscoveryTimeout,
			tailnetAddrs,
			opts.discoverSelf,
			validateAddresses,
			reporter.report,
		)
		if monitorErr != nil {
			cancelDaemon()
		}
		tailnetMonitorDone <- monitorErr
	}()
	serveErr := serveListeners(daemonCtx, cancelDaemon, listener, server.Handle)
	cancelDaemon()
	<-powerDone
	<-reconciled
	<-privateNamesDone
	<-publicCertificateDone
	<-publicHeartbeatDone
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

type edgeReportWriter struct{ reporter *errorReporter }

func (w edgeReportWriter) Write(contents []byte) (int, error) {
	message := strings.TrimSpace(string(contents))
	if message != "" {
		w.reporter.report(fmt.Errorf("daemon: public edge: %s", message))
	}
	return len(contents), nil
}
