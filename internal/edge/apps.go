package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sync"

	"github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/protocol"
)

func ValidateAppOriginEndpoint(endpoint netip.AddrPort, controlPort uint16) error {
	return validateOriginEndpoint(endpoint, controlPort)
}

// AppClientIP returns only the address validated by the public entrance.
func (r *Registry) AppClientIP(request *http.Request) netip.Addr {
	address, _ := request.Context().Value(proxyClientIPKey{}).(netip.Addr)
	return address
}

func (r *Registry) AcquireApp(request *http.Request, owner string) (func(), error) {
	if request.ContentLength > r.requestBodyLimit {
		return nil, errors.New("edge: app request body too large")
	}
	if _, err := parseIdentity("app owner", owner); err != nil {
		return nil, err
	}
	client := r.AppClientIP(request)
	if !client.IsValid() || !r.clients.Acquire(client) {
		return nil, errors.New("edge: app client concurrency limit exceeded")
	}
	select {
	case r.global <- struct{}{}:
	default:
		r.clients.Release(client)
		return nil, errors.New("edge: app global concurrency limit exceeded")
	}
	r.budgetsMu.Lock()
	budget := r.budgets[owner]
	if budget == nil {
		budget = make(chan struct{}, maximumConcurrentPerOrigin)
		r.budgets[owner] = budget
	}
	r.budgetsMu.Unlock()
	select {
	case budget <- struct{}{}:
	default:
		<-r.global
		r.clients.Release(client)
		return nil, errors.New("edge: app origin concurrency limit exceeded")
	}
	request.Body = &inboundRequestBody{ReadCloser: http.MaxBytesReader(nil, request.Body, r.requestBodyLimit)}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-budget
			<-r.global
			r.clients.Release(client)
		})
	}, nil
}

// AppExchange sends a pre-signed operation through the configured pinned edge.
// This method never signs a caller's operation.
func (p *Publisher) AppExchange(ctx context.Context, signed apps.Signed) (apps.Signed, error) {
	if ctx == nil {
		return apps.Signed{}, errors.New("edge: nil app context")
	}
	if err := signed.Verify("mesh-app/request/v1", p.target.Identity, p.originID, p.now()); err != nil {
		return apps.Signed{}, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	endpoint, err := p.resolve(operationCtx, p.target)
	if err != nil {
		return apps.Signed{}, err
	}
	if err := validateTargetEndpoint(endpoint, p.target.ControlPort); err != nil {
		return apps.Signed{}, err
	}
	connection, err := p.dial(operationCtx, "ws://"+endpoint.String()+p.target.WebSocketPath)
	if err != nil {
		return apps.Signed{}, err
	}
	defer connection.Close() //nolint:errcheck // the authenticated exchange result is authoritative
	requestID, err := registrationRequestID()
	if err != nil {
		return apps.Signed{}, err
	}
	host, err := registrationRoundTrip(operationCtx, connection, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		return apps.Signed{}, err
	}
	if host.Type != protocol.TypeHostInfoResult || host.Host == nil || host.Host.ID != p.target.Identity || host.Host.MeshIdentity != p.target.Identity {
		return apps.Signed{}, errors.New("edge: app edge identity does not match its pin")
	}
	p.publishPinnedAddress(endpoint.Addr())
	payload, err := json.Marshal(signed)
	if err != nil {
		return apps.Signed{}, err
	}
	requestID, err = registrationRequestID()
	if err != nil {
		return apps.Signed{}, err
	}
	response, err := registrationRoundTrip(operationCtx, connection, protocol.Control{Type: protocol.TypeAppEdge, RequestID: requestID, App: payload})
	if err != nil {
		return apps.Signed{}, err
	}
	if response.Type == protocol.TypeError {
		return apps.Signed{}, fmt.Errorf("edge: app operation rejected: %s", response.Message)
	}
	if response.Type != protocol.TypeAppEdge {
		return apps.Signed{}, errors.New("edge: invalid app exchange response")
	}
	var reply apps.Signed
	if err := json.Unmarshal(response.App, &reply); err != nil {
		return apps.Signed{}, err
	}
	if err := reply.Verify("mesh-app/response/v1", p.originID, p.target.Identity, p.now()); err != nil {
		return apps.Signed{}, err
	}
	if reply.Sequence != signed.Sequence {
		return apps.Signed{}, errors.New("edge: app exchange sequence does not match")
	}
	return reply, nil
}
