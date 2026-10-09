package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/inhibit"
	"github.com/shaul/mesh/internal/machinename"
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
// TailnetPort and SSHPort values disable their corresponding listeners. Zero
// connection caps select the defaults for Unix and Tailnet independently.
type Config struct {
	SubscriberLimit             int
	UnixConnectionLimit         int
	TailnetConnectionLimit      int
	SSHSessionHandler           sshd.SessionHandlerFactory
	WakePeerIdentity            func(context.Context, string) (string, error)
	StateDir                    string
	TailnetPort                 uint16
	SSHPort                     uint16
	WebSocketPath               string
	HTTPSPort                   uint16
	TailscaleServePort          uint16
	TailscaleServeProxyProtocol bool
	CertificateRenewerID        string
	PrivateNamesConfig          string
	AppRegistryConfig           string
	AppRegistryTarget           string
	AppDataRoot                 string
	TailscaleServe              bool
	// HibernateIdle stops a registered agent once its session has been
	// detached and quiet this long. Zero leaves every session running.
	HibernateIdle time.Duration
	ReportError   func(error)
}

type runOptions struct {
	listen                  func(string, string) (net.Listener, error)
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
	if cfg.TailscaleServeProxyProtocol && cfg.HTTPSPort == 0 {
		return errors.New("daemon: Tailscale PROXY metadata requires private HTTPS")
	}
	if cfg.TailscaleServePort != 0 && cfg.HTTPSPort == 0 {
		return errors.New("daemon: a Tailscale Serve gateway requires private HTTPS")
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
	var registryConfig *apps.RegistryHostConfig
	if cfg.AppRegistryConfig != "" {
		loaded, err := apps.LoadRegistryConfig(cfg.AppRegistryConfig)
		if err != nil {
			return fmt.Errorf("daemon: configure private app registry: %w", err)
		}
		registryConfig = &loaded
	}
	var appRegistryTarget *apps.Peer
	if cfg.AppRegistryTarget != "" {
		loaded, err := apps.LoadTargetConfig(cfg.AppRegistryTarget)
		if err != nil {
			return fmt.Errorf("daemon: configure private app registry target: %w", err)
		}
		appRegistryTarget = &loaded
	}
	networkRoles := registryConfig != nil || appRegistryTarget != nil
	requiresStableTailnetControl := cfg.TailscaleServe || networkRoles
	if networkRoles && opts.discoverPeers == nil {
		return errors.New("daemon: configured registry roles require Tailscale peer discovery")
	}
	if requiresStableTailnetControl && (opts.tailnetPollInterval <= 0 || opts.tailnetDiscoveryTimeout <= 0) {
		return errors.New("daemon: stable Tailnet control requires positive discovery and monitor timeouts")
	}
	if networkRoles && cfg.TailnetPort == 0 {
		return errors.New("daemon: configured registry roles require a non-zero Tailnet control port")
	}

	if cfg.SubscriberLimit < 0 {
		return errors.New("daemon: subscriber limit must be nonnegative")
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
	power, err := newWakeController(daemonCtx, stateDir, meshPrivateKey, opts.discoverPeers, cfg.WakePeerIdentity)
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
			reporter.report(fmt.Errorf("daemon: discover local Tailscale peer for private registry: %w", selfErr))
			return peers, nil
		}
		if peersErr != nil {
			reporter.report(fmt.Errorf("daemon: discover remote Tailscale peers for private registry: %w", peersErr))
			return []tailnet.Peer{self}, nil
		}
		return append([]tailnet.Peer{self}, peers...), nil
	}
	var tailnetAddrs []string
	var tailnetNames []string
	var tailscaleName *string
	if cfg.TailnetPort != 0 || cfg.SSHPort != 0 {
		disabledListeners := "Tailnet control listener"
		if cfg.SSHPort != 0 {
			disabledListeners = "SSH listener"
			if cfg.TailnetPort != 0 {
				disabledListeners = "Tailnet control and SSH listeners"
			}
		}
		// Bound discovery always, not only for the registry roles. A
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
				return fmt.Errorf("daemon: discover Tailscale addresses required by configured private registry networking: %w", discoverErr)
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
				shortName, _, _ := strings.Cut(name, ".")
				tailnetNames = []string{name, shortName}
			}
			if len(tailnetAddrs) == 0 {
				if requiresStableTailnetControl {
					return errors.New("daemon: configured private registry networking requires at least one discovered Tailscale address")
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

	osName, _ := os.Hostname()
	initialName := osName
	if tailscaleName != nil {
		initialName = *tailscaleName
	}
	names, err := machinename.Open(daemonCtx, stateDir, meshHost.ID, machinename.Initial(meshHost.ID, initialName, osName))
	if err != nil {
		return fmt.Errorf("daemon: load machine name: %w", err)
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
	var appRegistry *apps.Registry
	var certificateRuntime certificateRuntime
	var appRegistryHTTPHandler http.Handler
	var appRegistryListenAddress, appRegistryRenewerID string
	if registryConfig != nil {
		origins := make(map[string]string, len(registryConfig.Origins))
		allowed := make(map[string]bool, len(registryConfig.Origins))
		for _, origin := range registryConfig.Origins {
			origins[origin.TailscaleName] = origin.Identity
			allowed[origin.Identity] = true
		}
		networkOwners := tailnet.OwnerResolver(origins)
		appRegistry, err = apps.NewRegistry(daemonCtx, apps.RegistryConfig{
			ViewHostReady: func(host string) bool { return certificateRuntime.viewHostReady(host, opts.now()) },
			Store:         store, Key: meshPrivateKey, Allowed: allowed, Now: opts.now,
			Resolve:  appResolver(registryConfig.Origins, apps.TailscaleResolver(discoverAllPeers), apps.ControlPinner(nil)),
			ClientIP: appClientAddress, NetworkOwners: networkOwners,
		})
		if err != nil {
			return fmt.Errorf("daemon: configure private app registry: %w", err)
		}
		appRegistryHTTPHandler = privateAppsHTTPHandler(appRegistry, networkOwners)
		appRegistryListenAddress = registryConfig.ListenAddress
		appRegistryRenewerID = registryConfig.CertificateRenewerID
	}
	var appClient *apps.RegistryClient
	if appRegistryTarget != nil {
		appClient, err = apps.NewRegistryClient(apps.RegistryClientConfig{Key: meshPrivateKey, Target: *appRegistryTarget, Peers: discoverAllPeers, Now: opts.now, RequestTimeout: 5 * time.Second})
		if err != nil {
			return fmt.Errorf("daemon: configure private app registry client: %w", err)
		}
	}
	state := newStateBroker(cfg.SubscriberLimit, opts.now)
	metrics := hostmetrics.New()
	serviceControl, err := newServiceController(daemonCtx, homeDir, store, serviceRegistry)
	if err != nil {
		return err
	}
	certificateRuntime, err = configureCertificates(certificateRuntimeConfig{
		StateDir: stateDir, TargetID: meshHost.ID, OriginHTTPSPort: cfg.HTTPSPort,
		OriginRenewerID: cfg.CertificateRenewerID, AppRegistryRenewerID: appRegistryRenewerID,
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

	serviceControl.onCommitted = state.servicesCommitted
	serviceControl.publishCommitted()
	inhibitor := inhibit.New(reporter.report)
	defer func() { runErr = errors.Join(runErr, inhibitor.Close()) }()
	catalog, err := NewCatalog(CatalogConfig{
		OnReconcile:   syncSleepInhibitor(inhibitor.Update),
		OnChange:      state.sessionsChanged,
		OnObservation: state.observeSessions,
		SessionsDir:   sessionsDir,
		Host:          host,
		Store:         store,
		Probe:         newUnixWorkerProbe(),
		BootID:        opts.bootID,
		Now:           opts.now,
	})
	if err != nil {
		return err
	}
	if err := catalog.Reconcile(daemonCtx); err != nil {
		return err
	}
	state.seedSessions(catalog.previous)
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(daemonCtx), 5*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, catalog.FlushHost(flushCtx))
	}()
	connector, err := newWorkerConnector(sessionsDir, catalog)
	if err != nil {
		return err
	}
	lifecycle, err := newLifecycle(lifecycleConfig{
		Names:       names,
		NameChanged: state.hostChanged,
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
	state.hostChanged(lifecycle.declaredHostInfo())
	var appLocal *apps.Origin
	if appClient != nil {
		appLocal, err = apps.NewOrigin(daemonCtx, apps.OriginConfig{
			Store: store, Key: meshPrivateKey, RegistryIdentity: appRegistryTarget.Identity,
			Exchange: appClient.Exchange, Workers: appWorkers{lifecycle: lifecycle},
			DataRoot: appDataRoot(cfg.AppDataRoot, stateDir), Now: opts.now, CheckHosting: checkAppHosting(serviceRegistry),
		})
		if err != nil {
			return fmt.Errorf("daemon: configure temporary app origin: %w", err)
		}
		serviceControl.guard = appServiceGuard(appLocal)
	}
	demand := newDemandManager(daemonCtx, lifecycle, reporter.report)
	defer demand.Close()
	serviceControl.demand = demand
	demand.onChange = state.serviceDemandChanged
	serviceRegistry.SetDemandGate(demand, demand.logger)
	// Bind listeners and adopt sessions a previous daemon started before any
	// client can ask about them.
	demand.Sync(serviceRegistry.Services())
	serviceControl.publishCommitted()
	server, err := newClientServer(lifecycle, connector, serviceControl, certificateRuntime.Controller)
	if err != nil {
		return err
	}
	projectionDone := make(chan struct{})
	go func() { defer close(projectionDone); state.runProjection(daemonCtx, lifecycle) }()
	defer func() { cancelDaemon(); <-projectionDone }()
	server.state = state
	server.metrics = metrics
	memoryDone := make(chan struct{})
	go func() { defer close(memoryDone); state.runMemory(daemonCtx, lifecycle) }()
	defer func() { cancelDaemon(); <-memoryDone }()
	metricsDone := make(chan struct{})
	go func() { defer close(metricsDone); metrics.Run(daemonCtx, state.metricsChanged) }()
	defer func() { cancelDaemon(); <-metricsDone }()
	server.wake = power
	appControl := &appController{}
	if appLocal != nil {
		appControl.origin = appLocal
	}
	if appRegistry != nil {
		appControl.registry = appRegistry
	}
	server.apps = appControl
	updates, err := newUpdateController(stateDir, meshHost.ID, meshPrivateKey)
	if err != nil {
		return err
	}
	server.updates = updates

	controlAddrs := tailnetAddrs
	if cfg.TailnetPort == 0 {
		controlAddrs = nil
	}
	listener, err := validateListenerConfig(daemonCtx, ListenerConfig{
		StateDir:                   stateDir,
		UnixConnectionLimit:        cfg.UnixConnectionLimit,
		TailnetConnectionLimit:     cfg.TailnetConnectionLimit,
		TailnetAddrs:               controlAddrs,
		TailnetNames:               tailnetNames,
		PrivateName:                certificateRuntime.PrivateName,
		PrivateNames:               certificateRuntime.PrivateNames,
		PrivateServiceHost:         serviceRegistry.HasPrivateHost,
		TailnetPort:                cfg.TailnetPort,
		WebSocketPath:              cfg.WebSocketPath,
		HTTPHandler:                appOriginHandler(appLocal, serviceRegistry),
		HTTPSPort:                  cfg.HTTPSPort,
		HTTPSProxyProtocol:         cfg.TailscaleServeProxyProtocol,
		TLSConfig:                  certificateRuntime.OriginTLS,
		AppRegistryListenAddress:   appRegistryListenAddress,
		AppRegistryHTTPHandler:     appRegistryHTTPHandler,
		AppRegistryTLSConfig:       certificateRuntime.AppRegistryTLS,
		RequireAllTailnetListeners: requiresStableTailnetControl,
		ReportError:                reporter.report,
	}, server.Handle)
	if err != nil {
		return err
	}
	if opts.listen != nil {
		listener.listen = opts.listen
	}
	if cfg.SSHPort != 0 {
		var sessionHandler sshd.SessionHandler
		if cfg.SSHSessionHandler != nil {
			sessionHandler = cfg.SSHSessionHandler(stateDir)
		}
		for _, address := range sshAddrs {
			listener.sshConfigs = append(listener.sshConfigs, sshd.Config{
				Handler:        sessionHandler,
				Services:       serviceRegistry,
				HostKey:        meshPrivateKey,
				AuthorizedKeys: filepath.Join(stateDir, "authorized_keys"),
				Addr:           net.JoinHostPort(address, strconv.Itoa(int(cfg.SSHPort))),
			})
		}
		listener.serveSSH = opts.serveSSH
	}
	listenersReady := make(chan struct{})
	updatesDone := make(chan struct{})
	go func() {
		defer close(updatesDone)
		select {
		case <-listenersReady:
		case <-daemonCtx.Done():
			return
		}
		updates.coordinator.Run(daemonCtx, reporter.report)
	}()
	listener.ready = func(readyCtx context.Context) error {
		servePort := cfg.TailscaleServePort
		if servePort == 0 {
			servePort = cfg.HTTPSPort
		}
		if cfg.TailscaleServe {
			if err := configureTailscaleServe(readyCtx, servePort, cfg.TailscaleServeProxyProtocol, opts.tailscaleTimeout, opts.runCommand); err != nil {
				return err
			}
		}
		if cfg.HTTPSPort != 0 {
			verify := opts.verifyServeForward
			if cfg.TailscaleServeProxyProtocol {
				verify = tailnet.VerifyServeProxyForward
			}
			if err := verifyTailscaleServeForward(readyCtx, servePort, opts.tailscaleTimeout, verify); err != nil {
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
		reconcilePeriodically(daemonCtx, catalog, opts.reconcileInterval, reporter, func() {
			state.hostChanged(lifecycle.declaredHostInfo())
			if state.hasTopic("services") {
				serviceControl.observeRegistry(daemonCtx, state.observeServices)
			}
		})
	}()
	demandDone := make(chan struct{})
	go func() {
		defer close(demandDone)
		demand.Run(daemonCtx, demandSuperviseInterval)
	}()
	hibernated := make(chan struct{})
	go func() {
		defer close(hibernated)
		if cfg.HibernateIdle <= 0 {
			return
		}
		idle := &hibernator{lifecycle: lifecycle, idle: cfg.HibernateIdle, now: opts.now, refused: map[storage.SessionID]string{}}
		idle.run(daemonCtx, hibernationInterval(cfg.HibernateIdle))
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
		if err := privateNamesRuntime.Run(daemonCtx, func(err error) {
			reporter.report(fmt.Errorf("daemon: reconcile private names: %w", err))
		}); err != nil && daemonCtx.Err() == nil {
			reporter.report(fmt.Errorf("daemon: private-names loop: %w", err))
		}
	}()

	appsDone := make(chan struct{})
	go func() {
		defer close(appsDone)
		runAppMaintenance(daemonCtx, listenersReady, appLocal, appRegistry, reporter)
	}()
	tailnetMonitorDone := make(chan error, 1)
	// Watch Tailnet addresses whenever a listener depends on them, not only for
	// the registry roles. Startup discovery is one shot: if tailscaled
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
	<-updatesDone
	<-powerDone
	<-reconciled
	<-demandDone
	<-hibernated
	<-privateNamesDone
	<-appsDone
	return errors.Join(serveErr, <-tailnetMonitorDone)
}

func reconcilePeriodically(ctx context.Context, catalog *Catalog, interval time.Duration, reporter *errorReporter, observers ...func()) {
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
			for _, observe := range observers {
				observe()
			}
		}
	}
}
