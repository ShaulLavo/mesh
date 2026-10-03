package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

const AuthProtocol = "mesh-control-tls-v1"
const handshakeTimeout = 5 * time.Second
const handshakeByteLimit = 64 << 10

var ErrAuthentication = errors.New("transport: Mesh peer authentication failed")

var ErrAuthenticationRequired = errors.New("transport: peer requires a control-authentication upgrade")

// Authentication binds TLS to Mesh keys, never to certificate names or a public CA.
type Authentication struct {
	Key              ed25519.PrivateKey
	ExpectedIdentity string
	Authorize        func(string) bool
	Admit            func(string, protocol.Frame) bool
	// Other authority must not keep a revoked full-device socket alive.
	Retain func(string) func() bool
}

type AuthenticatedPeer struct{ Identity string }
type peerKey struct{}

func Peer(ctx context.Context) (AuthenticatedPeer, bool) {
	peer, ok := ctx.Value(peerKey{}).(AuthenticatedPeer)
	return peer, ok
}

func LocalAuthentication(expected string) (*Authentication, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return nil, fmt.Errorf("transport: local authentication: %w", err)
	}
	_, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return nil, fmt.Errorf("transport: local authentication: %w", err)
	}
	return &Authentication{Key: key, ExpectedIdentity: expected}, nil
}

func DialPinned(ctx context.Context, endpoint, expected string) (Conn, error) {
	auth, err := LocalAuthentication(expected)
	if err != nil {
		return nil, err
	}
	return DialOnce(ctx, endpoint, DialOptions{Auth: auth})
}

func (a *Authentication) validate(server bool) error {
	if a == nil || len(a.Key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(a.Key.Seed()), a.Key) {
		return fmt.Errorf("%w: valid Mesh signing key required", ErrAuthentication)
	}
	if server && a.Authorize == nil {
		return fmt.Errorf("%w: peer authorizer required", ErrAuthentication)
	}
	if !server {
		if _, err := identity.IdentityKey(a.ExpectedIdentity); err != nil {
			return fmt.Errorf("%w: pinned destination Mesh identity required: %w", ErrAuthentication, err)
		}
	}
	return nil
}

func (a *Authentication) config(server bool) (*tls.Config, error) {
	if err := a.validate(server); err != nil {
		return nil, fmt.Errorf("transport: local authentication: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("transport: certificate serial: %w", err)
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, a.Key.Public(), a.Key)
	if err != nil {
		return nil, fmt.Errorf("transport: wrap Mesh identity: %w", err)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates:           []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: a.Key}},
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true, //nolint:gosec // VerifyConnection checks the pinned key; CA/DNS trust is unrelated to Mesh identity.
		VerifyConnection:       a.verifyPeer(server),
	}
	if server {
		cfg.ClientAuth = tls.RequireAnyClientCert
	}
	return cfg, nil
}

func (a *Authentication) verifyPeer(server bool) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 {
			return errors.New("transport: exactly one Mesh certificate required")
		}
		cert := state.PeerCertificates[0]
		key, ok := cert.PublicKey.(ed25519.PublicKey)
		if !ok || cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) != nil {
			return errors.New("transport: invalid Mesh identity certificate")
		}
		id := base64.RawURLEncoding.EncodeToString(key)
		if server {
			if !a.Authorize(id) {
				return errors.New("transport: device key is not approved")
			}
			return nil
		}
		if id != a.ExpectedIdentity {
			return errors.New("transport: destination Mesh identity changed")
		}
		return nil
	}
}

type handshakeConn struct {
	net.Conn
	remaining int
}

func (c *handshakeConn) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		n, err := c.Conn.Read(p)
		return n, err //nolint:wrapcheck // TLS consumes the underlying EOF and network errors
	}
	if c.remaining == 0 {
		return 0, errors.New("transport: authentication byte limit exceeded")
	}
	if len(p) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.Conn.Read(p)
	c.remaining -= n
	return n, err //nolint:wrapcheck // TLS consumes the underlying EOF and network errors
}

func authenticate(ctx context.Context, ws *websocket.Conn, auth *Authentication, server bool) (*tls.Conn, AuthenticatedPeer, error) {
	cfg, err := auth.config(server)
	if err != nil {
		return nil, AuthenticatedPeer{}, err
	}
	lifetime := context.WithoutCancel(ctx)
	ws.SetReadLimit(handshakeByteLimit)
	raw := &handshakeConn{Conn: websocket.NetConn(lifetime, ws, websocket.MessageBinary), remaining: handshakeByteLimit}
	var secure *tls.Conn
	if server {
		secure = tls.Server(raw, cfg)
	} else {
		secure = tls.Client(raw, cfg)
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	stop := context.AfterFunc(handshakeCtx, func() { _ = ws.CloseNow() })
	defer stop()
	deadline, _ := handshakeCtx.Deadline()
	if err := secure.SetDeadline(deadline); err != nil {
		_ = ws.CloseNow()
		return nil, AuthenticatedPeer{}, fmt.Errorf("%w: set handshake deadline: %w", ErrAuthentication, err)
	}
	if err := secure.HandshakeContext(handshakeCtx); err != nil {
		_ = ws.CloseNow()
		return nil, AuthenticatedPeer{}, fmt.Errorf("%w: %w", ErrAuthentication, err)
	}
	if err := confirmAuthentication(secure, server); err != nil {
		_ = ws.CloseNow()
		return nil, AuthenticatedPeer{}, fmt.Errorf("%w: %w", ErrAuthentication, err)
	}
	if err := secure.SetDeadline(time.Time{}); err != nil {
		_ = ws.CloseNow()
		return nil, AuthenticatedPeer{}, fmt.Errorf("%w: clear handshake deadline: %w", ErrAuthentication, err)
	}
	raw.remaining = -1
	ws.SetReadLimit(protocol.MaxPayload + protocolHeaderSize)
	key := secure.ConnectionState().PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	return secure, AuthenticatedPeer{Identity: base64.RawURLEncoding.EncodeToString(key)}, nil
}

// TLS 1.3 can return to a client before the server checks its certificate.
// An encrypted acceptance byte makes denial permanent before Dial returns.
func confirmAuthentication(secure *tls.Conn, server bool) error {
	if server {
		if _, err := secure.Write([]byte{1}); err != nil {
			return fmt.Errorf("confirm approved Mesh peer: %w", err)
		}
		return nil
	}
	var accepted [1]byte
	if _, err := io.ReadFull(secure, accepted[:]); err != nil {
		return fmt.Errorf("read Mesh peer acceptance: %w", err)
	}
	if accepted[0] != 1 {
		return errors.New("transport: invalid Mesh peer acceptance")
	}
	return nil
}

func containsAuthProtocol(r *http.Request) bool {
	for _, value := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if strings.TrimSpace(value) == AuthProtocol {
			return true
		}
	}
	return false
}

type authorizedConn struct {
	Conn
	auth    *Authentication
	peer    AuthenticatedPeer
	allowed func() bool
}

func (c *authorizedConn) ReadFrame() (protocol.Frame, error) {
	frame, err := c.Conn.ReadFrame()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("transport: read authenticated frame: %w", err)
	}
	if !c.allowed() || c.auth.Admit != nil && !c.auth.Admit(c.peer.Identity, frame) {
		_ = c.Close()
		return protocol.Frame{}, errors.New("transport: device grant revoked or control denied")
	}
	return frame, nil
}

func (c *socketConn) watchAuthorization(allowed func() bool) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if !allowed() {
				c.fail(errors.New("transport: device grant revoked"))
				return
			}
		}
	}
}
