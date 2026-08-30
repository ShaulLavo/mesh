package edge

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

const (
	defaultPublisherTimeout   = 15 * time.Second
	maximumPublisherTimeout   = time.Minute
	publishedSnapshotLifetime = 5 * time.Minute
	publisherSendAttempts     = 2
)

// ErrRegistrationRejected means the edge definitively refused a snapshot.
// The origin retains that exact unacknowledged attempt until the desired
// routes change or the same snapshot is accepted.
var ErrRegistrationRejected = errors.New("edge: registration was rejected")

// TargetConfig is the exact identity and tailnet location of the public edge.
type TargetConfig struct {
	Identity      string `json:"identity"`
	TailscaleName string `json:"tailscaleName"`
	ControlPort   uint16 `json:"controlPort"`
	WebSocketPath string `json:"websocketPath"`
}

// ResolveTarget derives the public edge's numeric endpoint locally.
type ResolveTarget func(context.Context, TargetConfig) (netip.AddrPort, error)

// PublisherConfig fixes the origin signer, edge pin, durable outbox, and
// bounded control transport.
type PublisherConfig struct {
	Signer         ed25519.PrivateKey
	Target         TargetConfig
	State          OutboxStore
	Resolve        ResolveTarget
	Dial           func(context.Context, string) (transport.Conn, error)
	Now            func() time.Time
	RequestTimeout time.Duration
	OnPinned       func(netip.Addr)
}

// Publisher sends durable complete snapshots and exposes bounded edge.list.
type Publisher struct {
	signer   ed25519.PrivateKey
	originID string
	target   TargetConfig
	state    OutboxStore
	resolve  ResolveTarget
	dial     func(context.Context, string) (transport.Conn, error)
	now      func() time.Time
	timeout  time.Duration
	onPinned func(netip.Addr)
	gate     chan struct{}
}

// Enabled reports that this concrete publisher has complete trust anchors.
func (*Publisher) Enabled() bool { return true }

// NewPublisher validates every immutable outbound trust anchor.
func NewPublisher(config PublisherConfig) (*Publisher, error) {
	if len(config.Signer) != ed25519.PrivateKeySize {
		return nil, errors.New("edge: publisher signer is not an Ed25519 private key")
	}
	if err := validateTargetConfig(config.Target); err != nil {
		return nil, err
	}
	if config.State == nil || config.Resolve == nil {
		return nil, errors.New("edge: publisher dependency is nil")
	}
	if config.Dial == nil {
		config.Dial = func(ctx context.Context, endpoint string) (transport.Conn, error) {
			return transport.DialOnce(ctx, endpoint, transport.DialOptions{})
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultPublisherTimeout
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout > maximumPublisherTimeout {
		return nil, fmt.Errorf("edge: publisher timeout %s is outside (0,%s]", config.RequestTimeout, maximumPublisherTimeout)
	}
	originID := base64.RawURLEncoding.EncodeToString(config.Signer.Public().(ed25519.PublicKey))
	return &Publisher{
		signer: append(ed25519.PrivateKey(nil), config.Signer...), originID: originID, target: config.Target,
		state: config.State, resolve: config.Resolve, dial: config.Dial, now: config.Now,
		timeout: config.RequestTimeout, onPinned: config.OnPinned, gate: make(chan struct{}, 1),
	}, nil
}

// Converge consumes a durable sequence and waits for the edge's exact durable
// acknowledgement. An exact pending desired state is retried idempotently;
// changed desired state supersedes an ambiguous attempt at a higher sequence.
func (p *Publisher) Converge(ctx context.Context, services []serve.Service) error {
	if ctx == nil {
		return errors.New("edge: nil publish context")
	}
	operationContext, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-operationContext.Done():
		return operationContext.Err()
	}

	desiredRoutes, err := routesFromServices(services)
	if err != nil {
		return err
	}
	current, err := p.loadOutbox(operationContext)
	if err != nil && !errors.Is(err, ErrSnapshotNotFound) {
		return err
	}
	if err == nil && !current.Acknowledged && routesEqual(current.Snapshot.Routes, desiredRoutes) &&
		current.Snapshot.ExpiresAt.After(p.now().UTC().Add(p.timeout)) {
		retryErr := p.sendAndSettle(operationContext, current)
		if retryErr != nil && !errors.Is(retryErr, ErrStaleSequence) && !errors.Is(retryErr, ErrSequenceConflict) {
			return retryErr
		}
		current.Acknowledged = retryErr == nil
	}
	sequence := uint64(1)
	if err == nil {
		if current.Snapshot.Sequence >= math.MaxInt64 {
			return errors.New("edge: outbound snapshot sequence is exhausted")
		}
		sequence = current.Snapshot.Sequence + 1
	}
	issuedAt := p.now().UTC()
	snapshot, err := SignSnapshot(NewSnapshot(
		p.target.Identity, p.originID, sequence, issuedAt, issuedAt.Add(publishedSnapshotLifetime), desiredRoutes,
	), p.signer, issuedAt)
	if err != nil {
		return err
	}
	digest, err := VerifySnapshot(snapshot, p.target.Identity, p.originID)
	if err != nil {
		return err
	}
	record := OutboxRecord{Snapshot: snapshot, Digest: digest}
	if err := p.state.SaveEdgeOutbox(operationContext, record); err != nil {
		return err
	}
	return p.sendAndSettle(operationContext, record)
}

func (p *Publisher) loadOutbox(ctx context.Context) (OutboxRecord, error) {
	record, err := p.state.LoadEdgeOutbox(ctx, p.target.Identity)
	if err != nil {
		return OutboxRecord{}, err
	}
	digest, err := VerifySnapshot(record.Snapshot, p.target.Identity, p.originID)
	if err != nil || digest != record.Digest {
		return OutboxRecord{}, errors.New("edge: durable outbox does not match the configured target and origin")
	}
	return record, nil
}

func (p *Publisher) sendAndSettle(ctx context.Context, record OutboxRecord) error {
	var lastError error
	for range publisherSendAttempts {
		lastError = p.sendSnapshot(ctx, record)
		if lastError == nil {
			return p.state.AcknowledgeEdgeOutbox(ctx, p.target.Identity, record.Snapshot.Sequence, record.Digest)
		}
		if isDefinitiveRegistrationError(lastError) {
			return lastError
		}
		if ctx.Err() != nil {
			break
		}
	}
	return lastError
}

func (p *Publisher) sendSnapshot(ctx context.Context, record OutboxRecord) error {
	endpoint, err := p.resolve(ctx, p.target)
	if err != nil {
		return fmt.Errorf("edge: resolve public edge: %w", err)
	}
	if err := validateTargetEndpoint(endpoint, p.target.ControlPort); err != nil {
		return err
	}
	connection, err := p.dial(ctx, "ws://"+endpoint.String()+p.target.WebSocketPath)
	if err != nil {
		return err
	}
	defer connection.Close()
	requestID, err := registrationRequestID()
	if err != nil {
		return err
	}
	hostResponse, err := registrationRoundTrip(ctx, connection, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		return err
	}
	if hostResponse.Type != protocol.TypeHostInfoResult || hostResponse.Host == nil ||
		hostResponse.Host.ID != p.target.Identity || hostResponse.Host.MeshIdentity != p.target.Identity {
		return errors.New("edge: public edge host.info identity does not match its pin")
	}
	p.publishPinnedAddress(endpoint.Addr())
	requestID, err = registrationRequestID()
	if err != nil {
		return err
	}
	wireSnapshot := snapshotToProtocol(record.Snapshot)
	response, err := registrationRoundTrip(ctx, connection, protocol.Control{
		Type: protocol.TypeEdgeRegister, RequestID: requestID, EdgeSnapshot: &wireSnapshot,
	})
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		switch response.ErrorCode {
		case protocol.ErrorCodeEdgeRouteCollision:
			return ErrRouteCollision
		case protocol.ErrorCodeEdgeStaleSequence:
			return ErrStaleSequence
		case protocol.ErrorCodeEdgeConflict:
			return ErrSequenceConflict
		case protocol.ErrorCodeEdgeWakeUnavailable:
			return ErrWakeUnavailable
		default:
			return ErrRegistrationRejected
		}
	}
	if response.Type != protocol.TypeEdgeRegistered || response.EdgeSequence != record.Snapshot.Sequence || response.EdgeDigest != record.Digest {
		return errors.New("edge: registration acknowledgement does not match the durable snapshot")
	}
	return nil
}

func isDefinitiveRegistrationError(err error) bool {
	return errors.Is(err, ErrRegistrationRejected) || errors.Is(err, ErrRouteCollision) ||
		errors.Is(err, ErrStaleSequence) || errors.Is(err, ErrSequenceConflict) || errors.Is(err, ErrWakeUnavailable)
}

// ListPage returns one authenticated bounded status page from the pinned edge.
func (p *Publisher) ListPage(ctx context.Context, cursor string, limit int) ([]protocol.EdgeRouteInfo, string, error) {
	if ctx == nil {
		return nil, "", errors.New("edge: nil list context")
	}
	if len(cursor) > maximumListCursorLength {
		return nil, "", errors.New("edge: list cursor is too long")
	}
	var cursorName, cursorService string
	if cursor != "" {
		var err error
		cursorName, cursorService, err = decodeListCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		cursorService = strings.TrimPrefix(cursorService, "/")
	}
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maximumListLimit {
		return nil, "", fmt.Errorf("edge: list limit %d is outside 1..%d", limit, maximumListLimit)
	}
	operationContext, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-operationContext.Done():
		return nil, "", operationContext.Err()
	}
	endpoint, err := p.resolve(operationContext, p.target)
	if err != nil {
		return nil, "", err
	}
	if err := validateTargetEndpoint(endpoint, p.target.ControlPort); err != nil {
		return nil, "", err
	}
	connection, err := p.dial(operationContext, "ws://"+endpoint.String()+p.target.WebSocketPath)
	if err != nil {
		return nil, "", err
	}
	defer connection.Close()
	requestID, err := registrationRequestID()
	if err != nil {
		return nil, "", err
	}
	hostResponse, err := registrationRoundTrip(operationContext, connection, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil || hostResponse.Type != protocol.TypeHostInfoResult || hostResponse.Host == nil || hostResponse.Host.ID != p.target.Identity || hostResponse.Host.MeshIdentity != p.target.Identity {
		return nil, "", errors.New("edge: public edge host.info identity does not match its pin")
	}
	p.publishPinnedAddress(endpoint.Addr())
	requestID, err = registrationRequestID()
	if err != nil {
		return nil, "", err
	}
	proof, err := signListProof(p.target.Identity, p.originID, requestID, cursor, limit, p.now().UTC(), p.signer)
	if err != nil {
		return nil, "", err
	}
	wireProof := listProofToProtocol(proof)
	response, err := registrationRoundTrip(operationContext, connection, protocol.Control{
		Type: protocol.TypeEdgeList, RequestID: requestID, EdgeCursor: cursor, EdgeLimit: limit, EdgeListProof: &wireProof,
	})
	if err != nil {
		return nil, "", err
	}
	if response.Type == protocol.TypeError {
		return nil, "", ErrRegistrationRejected
	}
	if response.Type != protocol.TypeEdgeListed || len(response.EdgeRoutes) > limit {
		return nil, "", errors.New("edge: invalid edge.list response")
	}
	var previous Route
	for index, route := range response.EdgeRoutes {
		if err := validateListedRoute(route); err != nil {
			return nil, "", err
		}
		current := Route{PublicName: route.PublicName, ServiceName: route.ServiceName, WakeOnRequest: route.WakeOnRequest}
		if cursor != "" && compareRoute(current, Route{PublicName: cursorName, ServiceName: cursorService}) <= 0 {
			return nil, "", errors.New("edge: edge.list route does not follow its cursor")
		}
		if index > 0 && compareRoute(previous, current) >= 0 {
			return nil, "", errors.New("edge: edge.list routes are not strictly ordered")
		}
		previous = current
	}
	if response.EdgeNextCursor != "" {
		if len(response.EdgeRoutes) == 0 || response.EdgeNextCursor != encodeListCursor(previous.PublicName, "/"+previous.ServiceName) {
			return nil, "", errors.New("edge: edge.list cursor does not identify the final route")
		}
	}
	return append([]protocol.EdgeRouteInfo(nil), response.EdgeRoutes...), response.EdgeNextCursor, nil
}

func (p *Publisher) publishPinnedAddress(address netip.Addr) {
	if p.onPinned != nil {
		p.onPinned(address.Unmap())
	}
}

func routesFromServices(services []serve.Service) ([]Route, error) {
	routes := make([]Route, 0, len(services))
	for _, service := range services {
		if service.PublicName == "" {
			continue
		}
		route := Route{PublicName: service.PublicName, ServiceName: service.Name, WakeOnRequest: service.WakeOnRequest}
		if err := validateRoute(route); err != nil {
			return nil, err
		}
		routes = append(routes, route)
		if len(routes) > MaximumRoutes {
			return nil, fmt.Errorf("edge: public service count exceeds %d", MaximumRoutes)
		}
	}
	slices.SortFunc(routes, compareRoute)
	for index := 1; index < len(routes); index++ {
		if compareRoute(routes[index-1], routes[index]) == 0 {
			return nil, errors.New("edge: public service routes are duplicated")
		}
	}
	return routes, nil
}

func compareRoute(left, right Route) int {
	if result := cmp.Compare(left.PublicName, right.PublicName); result != 0 {
		return result
	}
	return cmp.Compare(left.ServiceName, right.ServiceName)
}

func routesEqual(left, right []Route) bool {
	return slices.EqualFunc(left, right, func(a, b Route) bool { return a == b })
}

func validateTargetConfig(target TargetConfig) error {
	if _, err := parseIdentity("edge target", target.Identity); err != nil {
		return err
	}
	if err := validateTailscaleName(target.TailscaleName); err != nil {
		return err
	}
	if target.ControlPort == 0 {
		return errors.New("edge: target control port is zero")
	}
	if err := validateControlPath(target.WebSocketPath); err != nil {
		return err
	}
	return nil
}

func validateTargetEndpoint(endpoint netip.AddrPort, controlPort uint16) error {
	if err := validateOriginEndpoint(endpoint, controlPort); err != nil {
		return errors.New("edge: resolved public edge endpoint is not the configured numeric Tailscale control endpoint")
	}
	return nil
}

func validateListedRoute(route protocol.EdgeRouteInfo) error {
	if err := validateRoute(Route{PublicName: route.PublicName, ServiceName: route.ServiceName, WakeOnRequest: route.WakeOnRequest}); err != nil {
		return err
	}
	if err := validateDisplayAlias(route.DisplayAlias); err != nil {
		return err
	}
	if route.LastSeenAt.IsZero() {
		return errors.New("edge: listed route has no last-seen time")
	}
	return nil
}

// TailscaleTargetResolver resolves one exact public edge peer.
func TailscaleTargetResolver(peers func(context.Context) ([]tailnet.Peer, error)) ResolveTarget {
	return func(ctx context.Context, target TargetConfig) (netip.AddrPort, error) {
		return resolveTailscalePeer(ctx, peers, target.TailscaleName, target.ControlPort)
	}
}

func resolveTailscalePeer(ctx context.Context, peers func(context.Context) ([]tailnet.Peer, error), name string, port uint16) (netip.AddrPort, error) {
	if peers == nil {
		return netip.AddrPort{}, errors.New("edge: nil Tailscale peer discovery")
	}
	observed, err := peers(ctx)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var matched *tailnet.Peer
	for index := range observed {
		peer := &observed[index]
		if peer.Name != name {
			continue
		}
		if matched != nil {
			return netip.AddrPort{}, errors.New("edge: Tailscale returned a duplicate allowlisted peer name")
		}
		matched = peer
	}
	if matched == nil {
		return netip.AddrPort{}, errors.New("edge: allowlisted Tailscale peer was not found")
	}
	if !matched.Online {
		return netip.AddrPort{}, errors.New("edge: allowlisted peer is offline in Tailscale")
	}
	addresses := make([]netip.Addr, 0, len(matched.Addrs))
	for _, value := range matched.Addrs {
		address, err := netip.ParseAddr(value)
		if err != nil || !tailscaleIPv4.Contains(address.Unmap()) && !tailscaleIPv6.Contains(address.Unmap()) {
			return netip.AddrPort{}, errors.New("edge: Tailscale returned an invalid peer address")
		}
		addresses = append(addresses, address.Unmap())
	}
	if len(addresses) == 0 {
		return netip.AddrPort{}, errors.New("edge: allowlisted peer has no Tailscale address")
	}
	slices.SortFunc(addresses, func(left, right netip.Addr) int {
		if left.Is4() != right.Is4() {
			if left.Is4() {
				return -1
			}
			return 1
		}
		return left.Compare(right)
	})
	return netip.AddrPortFrom(addresses[0], port), nil
}
