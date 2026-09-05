package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/wake"
	"github.com/shaul/mesh/internal/wakeclient"
)

type localClientKey struct{}

type wakeAuthority interface {
	Current(context.Context) (wake.Grant, error)
	SetAllowed(context.Context, bool) (wake.Grant, error)
}

type wakeSender interface {
	Probe(context.Context, wake.Grant) (wake.State, error)
	Send(context.Context, wake.Grant) (bool, error)
}

type wakeController struct {
	authority wakeAuthority
	sender    wakeSender
	client    *wakeclient.Client
	cache     *wake.Cache
	slots     chan struct{}
	changed   chan struct{}
	mu        sync.Mutex
	current   *wake.Grant
}

func newWakeController(ctx context.Context, stateDir string, key ed25519.PrivateKey, peers func(context.Context) ([]tailnet.Peer, error)) (*wakeController, error) {
	authority, err := wake.NewAuthority(stateDir, key)
	if err != nil {
		return nil, err
	}
	sender, err := wake.NewSender(stateDir)
	if err != nil {
		return nil, err
	}
	if peers == nil {
		peers = func(context.Context) ([]tailnet.Peer, error) { return nil, nil }
	}
	client, err := wakeclient.New(stateDir, wakeclient.Options{DiscoverPeers: peers})
	if err != nil {
		return nil, err
	}
	cache, err := wake.NewCache(stateDir)
	if err != nil {
		return nil, err
	}
	controller := &wakeController{authority: authority, sender: sender, client: client, cache: cache, slots: make(chan struct{}, 16), changed: make(chan struct{}, 1)}
	// Missing NIC metadata must never prevent session startup.
	_ = controller.refresh(ctx)
	return controller, nil
}

func (w *wakeController) info() *wake.Grant {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == nil || !time.Now().Before(w.current.ExpiresAt) {
		return nil
	}
	copy := *w.current
	return &copy
}

func (w *wakeController) refresh(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	grant, err := w.authority.Current(bounded)
	if err != nil {
		return err
	}
	return w.rememberCurrent(ctx, grant)
}

func (w *wakeController) rememberCurrent(ctx context.Context, grant wake.Grant) error {
	if err := w.cache.PutContext(ctx, grant); err != nil && !errors.Is(err, wake.ErrStaleGrant) {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	latest, err := w.cache.Get(grant.TargetID)
	if err != nil {
		return err
	}
	w.current = &latest
	return nil
}

func (w *wakeController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	switch request.Type {
	case protocol.TypeWakeConfigure, protocol.TypeWakeProbe, protocol.TypeWakeSend, protocol.TypeWakeRemember:
	default:
		return protocol.Control{}, false, nil
	}
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, true, err
	}
	select {
	case w.slots <- struct{}{}:
		defer func() { <-w.slots }()
	default:
		return protocol.Control{}, true, errors.New("wake sender is busy")
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := w.control(bounded, request)
	response.RequestID = request.RequestID
	return response, true, err
}

func (w *wakeController) control(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if request.Type == protocol.TypeWakeConfigure {
		return w.configure(ctx, request)
	}
	if request.WakeGrant == nil {
		return protocol.Control{}, errors.New("wake request has no target permission")
	}
	grant := *request.WakeGrant
	if err := w.cache.PutContext(ctx, grant); err != nil {
		return protocol.Control{}, err
	}
	switch request.Type {
	case protocol.TypeWakeRemember:
		return protocol.Control{Type: protocol.TypeWakeRemembered}, nil
	case protocol.TypeWakeProbe:
		state, err := w.sender.Probe(ctx, grant)
		if err != nil {
			return protocol.Control{Type: protocol.TypeWakeProbed, WakeState: wake.Unknown}, nil
		}
		return protocol.Control{Type: protocol.TypeWakeProbed, WakeCanSend: true, WakeState: state}, nil
	case protocol.TypeWakeSend:
		_, err := w.sender.Send(ctx, grant)
		return protocol.Control{Type: protocol.TypeWakeSent}, err
	default:
		return protocol.Control{}, errors.New("unknown wake operation")
	}
}

func (w *wakeController) configure(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if local, _ := ctx.Value(localClientKey{}).(bool); !local {
		return protocol.Control{}, errors.New("change wake permission on the target's local daemon socket")
	}
	if request.WakeAllowed == nil {
		return protocol.Control{}, errors.New("wake configuration has no allow or deny choice")
	}
	grant, err := w.authority.SetAllowed(ctx, *request.WakeAllowed)
	if err != nil {
		return protocol.Control{}, err
	}
	if err := w.rememberCurrent(ctx, grant); err != nil {
		return protocol.Control{}, err
	}
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return protocol.Control{Type: protocol.TypeWakeConfigured, WakeGrant: &grant}, nil
}

func (w *wakeController) run(ctx context.Context, ready <-chan struct{}, advertise bool) {
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	w.sync(ctx, advertise)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.changed:
		}
		w.sync(ctx, advertise)
	}
}

func (w *wakeController) sync(ctx context.Context, advertise bool) {
	if err := w.refresh(ctx); err != nil {
		return
	}
	grant := w.info()
	if grant == nil || !advertise {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = w.client.Publish(bounded, *grant)
}

type edgeWakeAdapter struct {
	client  *wakeclient.Client
	origins []edge.OriginConfig
	resolve edge.ResolveOrigin
}

func (w *edgeWakeAdapter) Wake(ctx context.Context, id string) error {
	for _, origin := range w.origins {
		if origin.Identity != id {
			continue
		}
		endpoint, err := w.resolve(ctx, origin)
		if err != nil {
			return err
		}
		_, err = w.client.Wake(ctx, wakeTarget(origin, endpoint))
		return err
	}
	return fmt.Errorf("wake origin %s is not configured", id)
}

func (w *edgeWakeAdapter) pin(ctx context.Context, endpoint netip.AddrPort, origin edge.OriginConfig) error {
	return w.client.Refresh(ctx, wakeTarget(origin, endpoint))
}

func wakeTarget(origin edge.OriginConfig, endpoint netip.AddrPort) wakeclient.Target {
	return wakeclient.Target{ID: origin.Identity, Name: origin.TailscaleName,
		Endpoint: (&url.URL{Scheme: "ws", Host: endpoint.String(), Path: origin.WebSocketPath}).String()}
}
