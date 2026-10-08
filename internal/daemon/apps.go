package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/worker"
)

type appOrigin interface {
	Handle(context.Context, apps.Request) (apps.Result, error)
}

type appEdge interface {
	Exchange(context.Context, apps.Signed) (apps.Signed, error)
}

func networkOwnerRateExemption(resolve func(context.Context, netip.Addr) ([]string, error)) func(context.Context, netip.Addr) bool {
	if resolve == nil {
		return nil
	}
	return func(ctx context.Context, address netip.Addr) bool {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		owners, err := resolve(ctx, address)
		return err == nil && len(owners) != 0
	}
}

type appController struct {
	origin appOrigin
	edge   appEdge
}

func (c *appController) HandleControl(ctx context.Context, q protocol.Control) (protocol.Control, bool, error) {
	if q.Type != protocol.TypeAppRequest && q.Type != protocol.TypeAppEdge {
		return protocol.Control{}, false, nil
	}
	response := protocol.Control{Type: protocol.TypeAppResult, RequestID: q.RequestID}
	if q.Type == protocol.TypeAppEdge {
		response.Type = protocol.TypeAppEdge
		if c.edge == nil {
			return response, true, errors.New("app: this host is not a public edge")
		}
		var signed apps.Signed
		if err := decodeAppControl(q.App, &signed); err != nil {
			return response, true, err
		}
		reply, err := c.edge.Exchange(ctx, signed)
		if err != nil {
			return response, true, err
		}
		response.App, err = json.Marshal(reply)
		return response, true, err
	}
	if local, _ := ctx.Value(localClientKey{}).(bool); !local {
		return response, true, errors.New("app: owner operations require the host's local Unix socket")
	}
	if c.origin == nil {
		return response, true, errors.New("app: configure --public-edge-target on the owner host")
	}
	var request apps.Request
	if err := decodeAppControl(q.App, &request); err != nil {
		return response, true, err
	}
	result, err := c.origin.Handle(ctx, request)
	if err != nil {
		return response, true, err
	}
	response.App, err = json.Marshal(result)
	return response, true, err
}

func decodeAppControl(data []byte, value any) error {
	if len(data) == 0 || len(data) > 1<<20 {
		return errors.New("app: control payload exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("app: invalid control: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("app: trailing control data")
	}
	return nil
}

type appWorkers struct{ lifecycle *lifecycle }

func (w appWorkers) Start(ctx context.Context, label, command, cwd string, env []string) (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return w.lifecycle.startLabelled(ctx, label, []string{shell, "-lc", command}, cwd, env)
}

func (w appWorkers) Stop(ctx context.Context, id string) error {
	return w.lifecycle.stopSession(ctx, id)
}

func (w appWorkers) Find(ctx context.Context, label string) (string, bool, error) {
	return w.lifecycle.findLabelled(ctx, label)
}

func (w appWorkers) Forget(ctx context.Context, label string) {
	w.lifecycle.forgetLabelled(ctx, label, "")
}

func (w appWorkers) Processes(_ context.Context, id string) ([]int, error) {
	meta, err := worker.ReadMeta(filepath.Join(w.lifecycle.sessionsDir, id))
	if err != nil {
		return nil, fmt.Errorf("session %s: read metadata: %w", id, err)
	}
	if meta.ID != id || meta.PID <= 0 {
		return nil, fmt.Errorf("session %s: no command process recorded", id)
	}
	processes, err := worker.SessionProcesses(id, meta.PID)
	if err != nil {
		return nil, fmt.Errorf("list app processes: %w", err)
	}
	return processes, nil
}

// Output is the bounded tail an owner sees when an app's setup fails.
func (w appWorkers) Output(ctx context.Context, id string) string {
	return w.lifecycle.outputTail(ctx, id)
}

func (w appWorkers) Wait(ctx context.Context, id string) (int, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(servedStopPoll)
	defer ticker.Stop()
	for {
		if exit, ended := w.lifecycle.sessionExit(id); ended {
			if exit == nil {
				return -1, errors.New("app: setup worker ended without an exit status")
			}
			return *exit, nil
		}
		select {
		case <-waitCtx.Done():
			return -1, fmt.Errorf("app: wait for setup: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func appDataRoot(configured, stateDir string) string {
	if configured != "" {
		return configured
	}
	if runtime.GOOS == "linux" {
		return "/work/mesh/apps"
	}
	return filepath.Join(stateDir, "apps-data")
}

func checkAppHosting(registry *meshserve.Registry) func(int, string) error {
	return func(port int, root string) error {
		return checkAppServiceSnapshot(registry.Services(), port, root)
	}
}

func checkAppServiceSnapshot(services []meshserve.Service, port int, root string) error {
	managedRoot, err := canonicalAppPath(root)
	if err != nil {
		return err
	}
	for _, service := range services {
		if err := checkAppServiceExposure(service, port, managedRoot); err != nil {
			return err
		}
	}
	return nil
}

func checkAppServiceExposure(service meshserve.Service, port int, managedRoot string) error {
	ports, root, err := appServiceAccess(service)
	if err != nil {
		return err
	}
	for _, used := range ports {
		if port != 0 && used == port {
			return errors.New("app: requested port is already exposed by an ordinary service")
		}
	}
	if root != "" && (appPathWithin(root, managedRoot) || appPathWithin(managedRoot, root)) {
		return errors.New("app: managed app directories overlap an ordinary file service")
	}
	return nil
}

func appServiceGuard(origin *apps.Origin) func(context.Context, meshserve.Service) (func(), error) {
	return func(ctx context.Context, service meshserve.Service) (func(), error) {
		ports, root, err := appServiceAccess(service)
		if err != nil {
			return nil, err
		}
		return origin.GuardService(ctx, ports, root)
	}
}

func appServiceAccess(service meshserve.Service) ([]int, string, error) {
	if service.Kind == meshserve.Static || service.Kind == meshserve.Files {
		root, err := canonicalAppPath(service.Target)
		return nil, root, err
	}
	if service.Kind != meshserve.Proxy {
		return nil, "", errors.New("app: unknown ordinary service kind")
	}
	port, err := strconv.Atoi(service.Target)
	if err != nil {
		return nil, "", fmt.Errorf("app: invalid ordinary service port: %w", err)
	}
	ports := []int{port}
	for _, listen := range service.Listens {
		ports = append(ports, int(listen.Public), int(listen.Upstream))
	}
	return ports, "", nil
}

func canonicalAppPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("app: resolve workload path: %w", err)
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			return "", fmt.Errorf("app: resolve workload path: %w", err)
		}
		missing = append([]string{filepath.Base(absolute)}, missing...)
		absolute = parent
	}
}

func appPathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func appOriginHandler(origin *apps.Origin, fallback http.Handler) http.Handler {
	if origin == nil {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !origin.ServeHTTP(w, r) {
			fallback.ServeHTTP(w, r)
		}
	})
}

func appResolver(origins []edge.OriginConfig, resolve edge.ResolveOrigin, pin edge.PinOrigin) func(context.Context, string) (netip.AddrPort, error) {
	byID := make(map[string]edge.OriginConfig, len(origins))
	for _, origin := range origins {
		byID[origin.Identity] = origin
	}
	return func(ctx context.Context, identity string) (netip.AddrPort, error) {
		origin, ok := byID[identity]
		if !ok {
			return netip.AddrPort{}, errors.New("app: origin identity is not configured")
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		endpoint, err := resolve(bounded, origin)
		if err != nil {
			return netip.AddrPort{}, err
		}
		if err := edge.ValidateAppOriginEndpoint(endpoint, origin.ControlPort); err != nil {
			return netip.AddrPort{}, err
		}
		if err := pin(bounded, endpoint, origin); err != nil {
			return netip.AddrPort{}, err
		}
		return endpoint, nil
	}
}

// appSyncInterval is how often the origin renews leases, well inside LeaseTTL.
// It is a variable only so integration builds can shorten it with the lease.
var appSyncInterval = 20 * time.Second

func runAppMaintenance(ctx context.Context, ready <-chan struct{}, origin *apps.Origin, public *apps.Edge, reporter *errorReporter) {
	if origin == nil && public == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	syncApps(ctx, origin, public, reporter)
	ticker := time.NewTicker(appSyncInterval)
	defer ticker.Stop()
	expiryTicker := time.NewTicker(time.Minute)
	defer expiryTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncApps(ctx, origin, nil, reporter)
		case <-expiryTicker.C:
			syncApps(ctx, nil, public, reporter)
		}
	}
}

func syncApps(ctx context.Context, origin *apps.Origin, public *apps.Edge, reporter *errorReporter) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if public != nil {
		if err := public.Sweep(bounded); err != nil && ctx.Err() == nil {
			reporter.report(fmt.Errorf("daemon: expire temporary apps: %w", err))
		}
	}
	// Sync budgets each of its steps itself; the shared deadline would leave its
	// safety stops a cancelled context after a slow lease exchange.
	if origin != nil {
		if err := origin.Sync(ctx); err != nil && ctx.Err() == nil {
			reporter.report(fmt.Errorf("daemon: reconcile temporary apps: %w", err))
		}
	}
}
