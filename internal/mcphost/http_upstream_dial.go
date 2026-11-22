package mcphost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const loopbackDialTimeout = 5 * time.Second

var (
	ipv4Loopback = netip.MustParseAddr("127.0.0.1")
	ipv6Loopback = netip.IPv6Loopback()
)

// httpEndpoint is the one approved upstream. Everything that touches the
// network or a credential goes through it, so the checks live in one place.
type httpEndpoint struct {
	url *url.URL
	// authority is the exact host:port the transport must ask us to dial.
	authority  string
	targets    []netip.AddrPort
	verifyDial func(ctx context.Context, target netip.AddrPort, dial func() (net.Conn, error)) (net.Conn, error)
}

// parseHTTPEndpoint accepts only plain http to 127.0.0.1, ::1 or localhost.
// Names other than localhost never reach a resolver, because a resolver
// answer is not proof of loopback.
func parseHTTPEndpoint(raw string) (httpEndpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return httpEndpoint{}, fmt.Errorf("parse MCP upstream URL: %w", errors.Unwrap(err))
	}
	if u.Scheme != "http" || u.Host == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return httpEndpoint{}, errors.New("MCP upstream URL must be a plain http URL without credentials or a fragment")
	}
	port, err := endpointPort(u)
	if err != nil {
		return httpEndpoint{}, err
	}
	targets, err := loopbackTargets(u.Hostname(), port)
	if err != nil {
		return httpEndpoint{}, err
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return httpEndpoint{url: u, authority: net.JoinHostPort(u.Hostname(), strconv.Itoa(int(port))), targets: targets}, nil
}

func endpointPort(u *url.URL) (uint16, error) {
	text := u.Port()
	if text == "" {
		return 80, nil
	}
	port, err := strconv.ParseUint(text, 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("MCP upstream URL has an invalid port")
	}
	return uint16(port), nil
}

func loopbackTargets(host string, port uint16) ([]netip.AddrPort, error) {
	if strings.EqualFold(host, "localhost") {
		return []netip.AddrPort{netip.AddrPortFrom(ipv4Loopback, port), netip.AddrPortFrom(ipv6Loopback, port)}, nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || (addr != ipv4Loopback && addr != ipv6Loopback) {
		return nil, errors.New("MCP upstream must be 127.0.0.1, [::1] or localhost")
	}
	return []netip.AddrPort{netip.AddrPortFrom(addr, port)}, nil
}

// dial ignores the address it is asked for unless it is the approved
// authority, and checks the connected peer, so no later change in the
// transport can send a request or a credential elsewhere.
func (e httpEndpoint) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" || !strings.EqualFold(address, e.authority) {
		return nil, notSent(errors.New("dial refused outside the approved MCP upstream"))
	}
	dialer := net.Dialer{Timeout: loopbackDialTimeout}
	var last error
	for _, target := range e.targets {
		conn, err := e.dialTarget(ctx, &dialer, target)
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, ErrManagedListenerMismatch) {
			return nil, notSent(err)
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, notSent(fmt.Errorf("connect to MCP upstream: %w", last))
}

func (e httpEndpoint) dialTarget(ctx context.Context, dialer *net.Dialer, target netip.AddrPort) (net.Conn, error) {
	dialOne := func() (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp", target.String())
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", target, err)
		}
		return e.verifiedPeer(conn, target)
	}
	if e.verifyDial != nil {
		return e.verifyDial(ctx, target, dialOne)
	}
	return dialOne()
}

func (e httpEndpoint) verifiedPeer(conn net.Conn, target netip.AddrPort) (net.Conn, error) {
	peer, ok := conn.RemoteAddr().(*net.TCPAddr)
	if ok && netip.AddrPortFrom(peer.AddrPort().Addr().Unmap(), peer.AddrPort().Port()) == target {
		return conn, nil
	}
	_ = conn.Close()
	return nil, notSent(errors.New("MCP upstream connection did not reach the approved loopback address"))
}
