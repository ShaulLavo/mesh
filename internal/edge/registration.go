package edge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

const (
	maximumOrigins         = 256
	defaultRegisterTimeout = 10 * time.Second
	maximumRegisterTimeout = time.Minute
)

var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// OriginConfig is one exact public-edge allowlist entry. DisplayAlias is the
// only field permitted in public error pages.
type OriginConfig struct {
	Identity      string `json:"identity"`
	DisplayAlias  string `json:"displayAlias"`
	TailscaleName string `json:"tailscaleName"`
	ControlPort   uint16 `json:"controlPort"`
	WebSocketPath string `json:"websocketPath"`
}

// ResolveOrigin derives one numeric endpoint from local Tailscale state.
type ResolveOrigin func(context.Context, OriginConfig) (netip.AddrPort, error)

// PinOrigin verifies host.info at a resolved numeric endpoint using the exact
// allowlisted control path and identity.
type PinOrigin func(context.Context, netip.AddrPort, OriginConfig) error

// ControllerConfig fixes every registration trust anchor at startup.
type ControllerConfig struct {
	TargetID       string
	Origins        []OriginConfig
	State          StateStore
	Registry       *Registry
	Resolve        ResolveOrigin
	Pin            PinOrigin
	Now            func() time.Time
	RequestTimeout time.Duration
}

// Controller authenticates, persists, and publishes complete origin state.
type Controller struct {
	targetID       string
	origins        map[string]OriginConfig
	state          StateStore
	registry       *Registry
	resolve        ResolveOrigin
	pin            PinOrigin
	now            func() time.Time
	timeout        time.Duration
	wakerAvailable bool
	lifetime       context.Context

	commitGate  chan struct{}
	live        map[string]liveOrigin
	originLocks map[string]chan struct{}
	dirty       bool
}

type liveOrigin struct {
	sequence uint64
	endpoint netip.AddrPort
}

// NewController validates the complete allowlist and restores durable claims
// offline before accepting traffic.
func NewController(ctx context.Context, config ControllerConfig) (*Controller, error) {
	if ctx == nil {
		return nil, errors.New("edge: nil controller startup context")
	}
	if _, err := parseIdentity("edge target", config.TargetID); err != nil {
		return nil, err
	}
	if config.State == nil || config.Registry == nil || config.Resolve == nil || config.Pin == nil {
		return nil, errors.New("edge: registration controller dependency is nil")
	}
	if len(config.Origins) == 0 || len(config.Origins) > maximumOrigins {
		return nil, fmt.Errorf("edge: origin allowlist count %d is outside 1..%d", len(config.Origins), maximumOrigins)
	}
	origins := make(map[string]OriginConfig, len(config.Origins))
	tailscaleNames := make(map[string]struct{}, len(config.Origins))
	for index, origin := range config.Origins {
		if err := validateOriginConfig(origin); err != nil {
			return nil, fmt.Errorf("edge: origin allowlist entry %d: %w", index, err)
		}
		if _, exists := origins[origin.Identity]; exists {
			return nil, errors.New("edge: origin allowlist contains a duplicate identity")
		}
		if _, exists := tailscaleNames[origin.TailscaleName]; exists {
			return nil, errors.New("edge: origin allowlist contains a duplicate Tailscale name")
		}
		origins[origin.Identity] = origin
		tailscaleNames[origin.TailscaleName] = struct{}{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRegisterTimeout
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout > maximumRegisterTimeout {
		return nil, fmt.Errorf("edge: registration timeout %s is outside (0,%s]", config.RequestTimeout, maximumRegisterTimeout)
	}
	controller := &Controller{
		targetID: config.TargetID, origins: origins, state: config.State, registry: config.Registry,
		resolve: config.Resolve, pin: config.Pin, now: config.Now, timeout: config.RequestTimeout,
		wakerAvailable: config.Registry.WakeAvailable(),
		lifetime:       ctx, commitGate: make(chan struct{}, 1), live: make(map[string]liveOrigin), originLocks: make(map[string]chan struct{}, len(origins)),
	}
	for identity := range origins {
		controller.originLocks[identity] = make(chan struct{}, 1)
	}
	controller.commitGate <- struct{}{}
	err := controller.publishLocked(ctx)
	<-controller.commitGate
	if err != nil {
		return nil, err
	}
	return controller, nil
}

// HandleControl implements edge.register, edge.registered, and edge.list.
func (c *Controller) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if ctx == nil {
		return protocol.Control{}, false, errors.New("edge: nil control context")
	}
	switch request.Type {
	case protocol.TypeEdgeRegister:
		if err := validateEdgeRequestID(request.RequestID); err != nil {
			return protocol.Control{}, true, err
		}
		if request.EdgeSnapshot == nil {
			return protocol.Control{}, true, errors.New("edge: edge.register requires a snapshot")
		}
		snapshot := snapshotFromProtocol(*request.EdgeSnapshot)
		origin, allowed := c.origins[snapshot.OriginID]
		if !allowed {
			return protocol.Control{}, true, errors.New("edge: origin identity is not allowlisted")
		}
		digest, err := VerifySnapshot(snapshot, c.targetID, origin.Identity)
		if err != nil {
			return protocol.Control{}, true, err
		}
		if err := validateSnapshotControlPath(snapshot, origin.WebSocketPath); err != nil {
			return protocol.Control{}, true, err
		}
		if !c.wakerAvailable {
			for _, route := range snapshot.Routes {
				if route.WakeOnRequest {
					return protocol.Control{}, true, ErrWakeUnavailable
				}
			}
		}
		if err := c.register(ctx, snapshot, digest, origin); err != nil {
			return protocol.Control{}, true, err
		}
		return protocol.Control{
			Type: protocol.TypeEdgeRegistered, RequestID: request.RequestID,
			EdgeSequence: snapshot.Sequence, EdgeDigest: digest,
		}, true, nil
	case protocol.TypeEdgeList:
		if err := validateEdgeRequestID(request.RequestID); err != nil {
			return protocol.Control{}, true, err
		}
		if request.EdgeListProof == nil {
			// A co-located origin first asks its service controller to sign and
			// forward this request. Only the forwarded form belongs to the edge.
			return protocol.Control{}, false, nil
		}
		proof := listProofFromProtocol(*request.EdgeListProof)
		if _, allowed := c.origins[proof.OriginID]; !allowed {
			return protocol.Control{}, true, errors.New("edge: list origin identity is not allowlisted")
		}
		if _, err := verifyListProof(proof, request.RequestID, request.EdgeCursor, request.EdgeLimit, c.targetID, c.now().UTC()); err != nil {
			return protocol.Control{}, true, err
		}
		statuses, nextCursor, err := c.registry.Page(request.EdgeCursor, request.EdgeLimit)
		if err != nil {
			return protocol.Control{}, true, err
		}
		listed := make([]protocol.EdgeRouteInfo, 0, len(statuses))
		for _, status := range statuses {
			listed = append(listed, protocol.EdgeRouteInfo{
				PublicName: status.PublicName, ServiceName: status.ServiceName, WakeOnRequest: status.WakeOnRequest,
				DisplayAlias: status.DisplayAlias, LastSeenAt: status.LastSeenAt, Online: status.Online,
			})
		}
		return protocol.Control{Type: protocol.TypeEdgeListed, RequestID: request.RequestID, EdgeRoutes: listed, EdgeNextCursor: nextCursor}, true, nil
	default:
		return protocol.Control{}, false, nil
	}
}

func (c *Controller) register(ctx context.Context, snapshot Snapshot, digest string, origin OriginConfig) error {
	operationContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	originGate := c.originLocks[snapshot.OriginID]
	select {
	case originGate <- struct{}{}:
		defer func() { <-originGate }()
	case <-operationContext.Done():
		return operationContext.Err()
	}

	if err := c.acquireCommit(operationContext); err != nil {
		return err
	}
	idempotent, err := c.classifyLocked(operationContext, snapshot, digest)
	if err != nil {
		c.releaseCommit()
		return err
	}
	if idempotent {
		if c.dirty {
			err = c.reconcileLocked()
		}
		c.releaseCommit()
		return err
	}
	c.releaseCommit()
	if err := ValidateFresh(snapshot, c.now().UTC()); err != nil {
		return err
	}
	endpoint, err := c.resolve(operationContext, origin)
	if err != nil {
		return fmt.Errorf("edge: resolve allowlisted origin: %w", err)
	}
	if err := validateOriginEndpoint(endpoint, origin.ControlPort); err != nil {
		return err
	}
	if err := c.pin(operationContext, endpoint, origin); err != nil {
		return fmt.Errorf("edge: pin allowlisted origin: %w", err)
	}

	if err := c.acquireCommit(operationContext); err != nil {
		return err
	}
	defer c.releaseCommit()
	idempotent, err = c.classifyLocked(operationContext, snapshot, digest)
	if err != nil {
		return err
	}
	if idempotent {
		if c.dirty {
			return c.reconcileLocked()
		}
		return nil
	}
	stored, err := c.state.LoadEdgeState(operationContext)
	if err != nil {
		return err
	}
	totalRoutes := len(snapshot.Routes)
	for _, existing := range stored {
		if existing.Snapshot.OriginID != snapshot.OriginID {
			totalRoutes += len(existing.Snapshot.Routes)
		}
	}
	if totalRoutes > MaximumTotalRoutes {
		return fmt.Errorf("edge: total route count %d exceeds %d", totalRoutes, MaximumTotalRoutes)
	}
	receivedAt := c.now().UTC()
	if err := ValidateFresh(snapshot, receivedAt); err != nil {
		return err
	}
	c.dirty = true
	if err := c.state.ApplyEdgeSnapshot(operationContext, snapshot, digest, receivedAt); err != nil {
		reconcileErr := c.reconcileAmbiguousApplyLocked(snapshot, digest, endpoint)
		return errors.Join(err, reconcileErr)
	}
	c.live[snapshot.OriginID] = liveOrigin{sequence: snapshot.Sequence, endpoint: endpoint}
	return c.reconcileLocked()
}

func (c *Controller) reconcileAmbiguousApplyLocked(snapshot Snapshot, digest string, endpoint netip.AddrPort) error {
	ctx, cancel := context.WithTimeout(c.lifetime, c.timeout)
	defer cancel()
	version, err := c.state.EdgeSnapshotVersion(ctx, snapshot.OriginID)
	if err == nil && version.Sequence == snapshot.Sequence && version.Digest == digest {
		c.live[snapshot.OriginID] = liveOrigin{sequence: snapshot.Sequence, endpoint: endpoint}
	} else if err != nil && !errors.Is(err, ErrSnapshotNotFound) {
		clearErr := c.registry.Replace(nil)
		return errors.Join(err, clearErr)
	} else if err == nil && version.Sequence == snapshot.Sequence && version.Digest != digest {
		clearErr := c.registry.Replace(nil)
		return errors.Join(ErrSequenceConflict, clearErr)
	}
	return c.reconcileLocked()
}

func (c *Controller) acquireCommit(ctx context.Context) error {
	select {
	case c.commitGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) releaseCommit() { <-c.commitGate }

func (c *Controller) classifyLocked(ctx context.Context, snapshot Snapshot, digest string) (bool, error) {
	current, err := c.state.EdgeSnapshotVersion(ctx, snapshot.OriginID)
	switch {
	case err == nil && current.Sequence > snapshot.Sequence:
		return false, ErrStaleSequence
	case err == nil && current.Sequence == snapshot.Sequence && current.Digest != digest:
		return false, ErrSequenceConflict
	case err == nil && current.Sequence == snapshot.Sequence:
		return true, nil
	case err != nil && !errors.Is(err, ErrSnapshotNotFound):
		return false, err
	default:
		return false, nil
	}
}

func (c *Controller) reconcileLocked() error {
	ctx, cancel := context.WithTimeout(c.lifetime, c.timeout)
	defer cancel()
	if err := c.publishLocked(ctx); err != nil {
		clearErr := c.registry.Replace(nil)
		return errors.Join(err, clearErr)
	}
	c.dirty = false
	return nil
}

func (c *Controller) publishLocked(ctx context.Context) error {
	state, err := c.state.LoadEdgeState(ctx)
	if err != nil {
		return err
	}
	routes := make([]PublishedRoute, 0)
	for _, stored := range state {
		origin, allowed := c.origins[stored.Snapshot.OriginID]
		if !allowed {
			// Removing a machine from the allowlist is how an operator revokes
			// it, and that is exactly when the edge must not refuse to start:
			// failing here took down sessions, tailnet listeners, private names
			// and SSH on the VPS, and crash-looped, leaving re-adding the
			// revoked machine as the only way back. Drop its claims instead.
			log.Printf("edge: dropping persisted claims for origin %s, which is no longer allowlisted", stored.Snapshot.OriginID)
			if err := c.state.DeleteEdgeOrigin(ctx, stored.Snapshot.OriginID); err != nil {
				return fmt.Errorf("edge: drop revoked origin %s: %w", stored.Snapshot.OriginID, err)
			}
			continue
		}
		digest, err := VerifySnapshot(stored.Snapshot, c.targetID, origin.Identity)
		if err != nil {
			return fmt.Errorf("edge: verify persisted snapshot: %w", err)
		}
		if digest != stored.Digest {
			return errors.New("edge: persisted snapshot digest does not match its contents")
		}
		if !c.wakerAvailable {
			for _, route := range stored.Snapshot.Routes {
				if route.WakeOnRequest {
					return ErrWakeUnavailable
				}
			}
		}
		resolved := ResolvedOrigin{
			Identity: origin.Identity, DisplayAlias: origin.DisplayAlias, LastSeenAt: stored.LastSeenAt,
			SnapshotSequence: stored.Snapshot.Sequence,
			OnlineUntil:      stored.LastSeenAt.Add(stored.Snapshot.ExpiresAt.Sub(stored.Snapshot.IssuedAt)),
		}
		if live, exists := c.live[origin.Identity]; exists && live.sequence == stored.Snapshot.Sequence {
			resolved.Endpoint = live.endpoint
			resolved.Online = true
		}
		for _, route := range stored.Snapshot.Routes {
			routes = append(routes, PublishedRoute{Route: route, Origin: resolved})
		}
	}
	return c.registry.Replace(routes)
}

func validateOriginConfig(origin OriginConfig) error {
	if _, err := parseIdentity("origin", origin.Identity); err != nil {
		return err
	}
	if err := validateDisplayAlias(origin.DisplayAlias); err != nil {
		return err
	}
	if err := validateTailscaleName(origin.TailscaleName); err != nil {
		return err
	}
	if origin.ControlPort == 0 {
		return errors.New("edge: origin control port is zero")
	}
	if err := validateControlPath(origin.WebSocketPath); err != nil {
		return err
	}
	return nil
}

func validateSnapshotControlPath(snapshot Snapshot, controlPath string) error {
	for _, route := range snapshot.Routes {
		prefix := "/" + route.ServiceName
		if prefix == controlPath || strings.HasPrefix(prefix, controlPath+"/") || strings.HasPrefix(controlPath, prefix+"/") {
			return errors.New("edge: public route overlaps the origin terminal control path")
		}
	}
	return nil
}

func validateTailscaleName(name string) error {
	if name == "" || len(name) > 253 || name != strings.ToLower(name) || strings.HasSuffix(name, ".") || strings.TrimSpace(name) != name {
		return errors.New("edge: Tailscale name is empty or not canonical")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("edge: Tailscale name has an invalid DNS label")
		}
		for _, character := range label {
			if character < 'a' || character > 'z' {
				if character < '0' || character > '9' {
					if character != '-' {
						return errors.New("edge: Tailscale name has an invalid DNS label")
					}
				}
			}
		}
	}
	return nil
}

func validateOriginEndpoint(endpoint netip.AddrPort, controlPort uint16) error {
	address := endpoint.Addr().Unmap()
	if !endpoint.IsValid() || endpoint.Port() != controlPort || !tailscaleIPv4.Contains(address) && !tailscaleIPv6.Contains(address) {
		return errors.New("edge: resolved origin endpoint is not the configured numeric Tailscale control endpoint")
	}
	return nil
}

func validateEdgeRequestID(value string) error {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return errors.New("edge: request ID is empty or invalid")
	}
	return nil
}

func snapshotFromProtocol(snapshot protocol.EdgeSnapshot) Snapshot {
	routes := make([]Route, len(snapshot.Routes))
	for index, route := range snapshot.Routes {
		routes[index] = Route{PublicName: route.PublicName, ServiceName: route.ServiceName, WakeOnRequest: route.WakeOnRequest}
	}
	return Snapshot{
		TargetID: snapshot.TargetID, OriginID: snapshot.OriginID, Sequence: snapshot.Sequence,
		IssuedAt: snapshot.IssuedAt, ExpiresAt: snapshot.ExpiresAt, Routes: routes, Signature: append([]byte(nil), snapshot.Signature...),
	}
}

func snapshotToProtocol(snapshot Snapshot) protocol.EdgeSnapshot {
	routes := make([]protocol.EdgeRoute, len(snapshot.Routes))
	for index, route := range snapshot.Routes {
		routes[index] = protocol.EdgeRoute{PublicName: route.PublicName, ServiceName: route.ServiceName, WakeOnRequest: route.WakeOnRequest}
	}
	return protocol.EdgeSnapshot{
		TargetID: snapshot.TargetID, OriginID: snapshot.OriginID, Sequence: snapshot.Sequence,
		IssuedAt: snapshot.IssuedAt, ExpiresAt: snapshot.ExpiresAt, Routes: routes, Signature: append([]byte(nil), snapshot.Signature...),
	}
}

// TailscaleResolver resolves exact allowlisted peer names and rejects all
// non-Tailscale addresses before returning a numeric endpoint.
func TailscaleResolver(peers func(context.Context) ([]tailnet.Peer, error)) ResolveOrigin {
	return func(ctx context.Context, origin OriginConfig) (netip.AddrPort, error) {
		return resolveTailscalePeer(ctx, peers, origin.TailscaleName, origin.ControlPort, true)
	}
}

// TailscaleWakeResolver retains the exact allowlisted peer's cached addresses
// while it sleeps. Readiness and identity are verified after the wake.
func TailscaleWakeResolver(peers func(context.Context) ([]tailnet.Peer, error)) ResolveOrigin {
	return func(ctx context.Context, origin OriginConfig) (netip.AddrPort, error) {
		return resolveTailscalePeer(ctx, peers, origin.TailscaleName, origin.ControlPort, false)
	}
}

// ControlPinner returns a host.info verifier that consumes the complete
// allowlisted origin entry, including its canonical WebSocket path.
func ControlPinner(dial func(context.Context, string) (transport.Conn, error)) PinOrigin {
	if dial == nil {
		dial = func(ctx context.Context, endpoint string) (transport.Conn, error) {
			return transport.DialOnce(ctx, endpoint, transport.DialOptions{})
		}
	}
	return func(ctx context.Context, endpoint netip.AddrPort, origin OriginConfig) error {
		if err := validateControlPath(origin.WebSocketPath); err != nil {
			return err
		}
		connection, err := dial(ctx, "ws://"+endpoint.String()+origin.WebSocketPath)
		if err != nil {
			return err
		}
		defer connection.Close() //nolint:errcheck // the registration exchange result is authoritative
		requestID, err := registrationRequestID()
		if err != nil {
			return err
		}
		response, err := registrationRoundTrip(ctx, connection, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
		if err != nil {
			return err
		}
		if response.Type != protocol.TypeHostInfoResult || response.Host == nil || response.Host.ID != origin.Identity || response.Host.MeshIdentity != origin.Identity {
			return errors.New("edge: host.info identity does not match the allowlist")
		}
		return nil
	}
}

func validateControlPath(webSocketPath string) error {
	parsedPath, err := url.Parse(webSocketPath)
	if err != nil || webSocketPath == "" || webSocketPath[0] != '/' || strings.Contains(webSocketPath, "\\") || path.Clean(webSocketPath) != webSocketPath ||
		parsedPath.Scheme != "" || parsedPath.Host != "" || parsedPath.User != nil || parsedPath.RawQuery != "" || parsedPath.Fragment != "" ||
		parsedPath.RawPath != "" || parsedPath.Opaque != "" || parsedPath.ForceQuery || parsedPath.EscapedPath() != webSocketPath {
		return errors.New("edge: WebSocket path is not canonical")
	}
	return nil
}

func registrationRoundTrip(ctx context.Context, connection transport.Conn, request protocol.Control) (protocol.Control, error) {
	payload, err := request.Encode()
	if err != nil {
		return protocol.Control{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := connection.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.Control{}, err
	}
	frame, err := connection.ReadFrame()
	if err != nil {
		if ctx.Err() != nil {
			return protocol.Control{}, ctx.Err()
		}
		return protocol.Control{}, err
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("edge: response is not a control frame")
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, err
	}
	if response.RequestID != request.RequestID {
		return protocol.Control{}, errors.New("edge: response request ID does not match")
	}
	return response, nil
}

func registrationRequestID() (string, error) {
	var contents [16]byte
	if _, err := io.ReadFull(rand.Reader, contents[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(contents[:]), nil
}
