package apps

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

type RegistryClientConfig struct {
	Key            ed25519.PrivateKey
	Target         Peer
	Peers          func(context.Context) ([]tailnet.Peer, error)
	Dial           func(context.Context, string) (transport.Conn, error)
	Now            func() time.Time
	RequestTimeout time.Duration
}
type RegistryClient struct {
	originID string
	target   Peer
	peers    func(context.Context) ([]tailnet.Peer, error)
	dial     func(context.Context, string) (transport.Conn, error)
	now      func() time.Time
	timeout  time.Duration
}

func NewRegistryClient(config RegistryClientConfig) (*RegistryClient, error) {
	if len(config.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("app: registry client key is not an Ed25519 private key")
	}
	if err := validatePeer(config.Target); err != nil {
		return nil, err
	}
	if config.Peers == nil {
		return nil, errors.New("app: registry client requires peer discovery")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 15 * time.Second
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout > time.Minute {
		return nil, errors.New("app: registry request timeout must be positive and at most one minute")
	}
	if config.Dial == nil {
		key := append(ed25519.PrivateKey(nil), config.Key...)
		config.Dial = func(ctx context.Context, endpoint string) (transport.Conn, error) {
			return transport.DialOnce(ctx, endpoint, transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: config.Target.Identity}})
		}
	}
	return &RegistryClient{originID: base64.RawURLEncoding.EncodeToString(config.Key.Public().(ed25519.PublicKey)), target: config.Target, peers: config.Peers, dial: config.Dial, now: config.Now, timeout: config.RequestTimeout}, nil
}
func (p *RegistryClient) Exchange(ctx context.Context, signed Signed) (Signed, error) {
	if ctx == nil {
		return Signed{}, errors.New("app: nil app context")
	}
	if err := signed.Verify("mesh-app/request/v1", p.target.Identity, p.originID, p.now()); err != nil {
		return Signed{}, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	connection, err := p.connectRegistry(operationCtx)
	if err != nil {
		return Signed{}, err
	}
	defer connection.Close() //nolint:errcheck // the authenticated exchange result is authoritative
	payload, err := json.Marshal(signed)
	if err != nil {
		return Signed{}, fmt.Errorf("app: encode registry operation: %w", err)
	}
	requestID, err := controlRequestID()
	if err != nil {
		return Signed{}, err
	}
	response, err := controlRoundTrip(operationCtx, connection, protocol.Control{Type: protocol.TypeAppRegistry, RequestID: requestID, App: payload})
	if err != nil {
		return Signed{}, err
	}
	return p.verifyRegistryReply(response, signed.Sequence)
}
func (p *RegistryClient) connectRegistry(ctx context.Context) (transport.Conn, error) {
	endpoint, err := resolveTailscalePeer(ctx, p.peers, p.target.TailscaleName, p.target.ControlPort)
	if err != nil {
		return nil, err
	}
	if err := ValidateOriginEndpoint(endpoint, p.target.ControlPort); err != nil {
		return nil, err
	}
	connection, err := p.dial(ctx, "ws://"+endpoint.String()+p.target.WebSocketPath)
	if err != nil {
		return nil, fmt.Errorf("app: connect registry: %w", err)
	}
	if err := verifyControlIdentity(ctx, connection, p.target.Identity); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}
func (p *RegistryClient) verifyRegistryReply(response protocol.Control, sequence uint64) (Signed, error) {
	if response.Type == protocol.TypeError {
		return Signed{}, fmt.Errorf("app: app operation rejected: %s", response.Message)
	}
	if response.Type != protocol.TypeAppRegistry {
		return Signed{}, errors.New("app: invalid app exchange response")
	}
	var reply Signed
	if err := json.Unmarshal(response.App, &reply); err != nil {
		return Signed{}, fmt.Errorf("app: decode registry response: %w", err)
	}
	if err := reply.Verify("mesh-app/response/v1", p.originID, p.target.Identity, p.now()); err != nil {
		return Signed{}, err
	}
	if reply.Sequence != sequence {
		return Signed{}, errors.New("app: app exchange sequence does not match")
	}
	return reply, nil
}
