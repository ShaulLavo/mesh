package tailnet

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProxyListener accepts PROXY v1 metadata only from a loopback TCP forwarder.
// Enable it solely behind Tailscale Serve or a trusted local SNI gateway.
type ProxyListener struct {
	net.Listener
	AllowedUIDs []uint32
	peerUID     peerUIDLookup
}

type peerUIDLookup interface {
	PeerUID(net.Conn) (uint32, error)
}

func (l ProxyListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return NewProxyConn(c), nil
}

type proxyConn struct {
	net.Conn
	once   sync.Once
	reader *bufio.Reader
	source net.Addr
	err    error
}

// NewProxyConn consumes one mandatory PROXY v1 header before any TLS bytes.
func NewProxyConn(c net.Conn) net.Conn { return &proxyConn{Conn: c} }

func (c *proxyConn) initialize() {
	c.once.Do(func() {
		peer, err := netip.ParseAddrPort(c.Conn.RemoteAddr().String())
		if err != nil || !peer.Addr().IsLoopback() {
			c.err = errors.New("tailnet: PROXY sender must be loopback")
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		defer func() { _ = c.SetReadDeadline(time.Time{}) }()
		c.reader = bufio.NewReaderSize(c.Conn, 108)
		line, err := c.reader.ReadSlice('\n')
		if err != nil || len(line) > 108 || !strings.HasSuffix(string(line), "\r\n") {
			c.err = errors.New("tailnet: invalid PROXY header")
			return
		}
		source, err := parseProxySource(strings.TrimSuffix(string(line), "\r\n"))
		c.err = err
		if err == nil {
			c.source = net.TCPAddrFromAddrPort(source)
		}
	})
}

func (c *proxyConn) Read(p []byte) (int, error) {
	c.initialize()
	if c.err != nil {
		return 0, c.err
	}
	return c.reader.Read(p)
}

func (c *proxyConn) RemoteAddr() net.Addr {
	c.initialize()
	if c.err != nil {
		return c.Conn.RemoteAddr()
	}
	return c.source
}

func parseProxySource(line string) (netip.AddrPort, error) {
	fields := strings.Split(line, " ")
	if len(fields) != 6 || fields[0] != "PROXY" || (fields[1] != "TCP4" && fields[1] != "TCP6") {
		return netip.AddrPort{}, errors.New("tailnet: unsupported PROXY header")
	}
	source, err := netip.ParseAddr(fields[2])
	destination, destinationErr := netip.ParseAddr(fields[3])
	port, portErr := strconv.ParseUint(fields[4], 10, 16)
	destinationPort, destinationPortErr := strconv.ParseUint(fields[5], 10, 16)
	if err != nil || destinationErr != nil || portErr != nil || destinationPortErr != nil || port == 0 || destinationPort == 0 ||
		source.Is4() != destination.Is4() || source.Is4() != (fields[1] == "TCP4") || source.Is4In6() || destination.Is4In6() {
		return netip.AddrPort{}, errors.New("tailnet: invalid PROXY addresses")
	}
	return netip.AddrPortFrom(source, uint16(port)), nil
}

// WriteProxyHeader sends validated source metadata to another trusted listener.
func WriteProxyHeader(w io.Writer, source net.Addr, destination net.Addr) error {
	src, err := netip.ParseAddrPort(source.String())
	if err != nil {
		return err
	}
	dst, err := netip.ParseAddrPort(destination.String())
	if err != nil {
		return err
	}
	protocol := "TCP6"
	if src.Addr().Is4() {
		protocol = "TCP4"
		dst = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), dst.Port())
	}
	if !src.Addr().Is4() {
		dst = netip.AddrPortFrom(netip.IPv6Loopback(), dst.Port())
	}
	_, err = fmt.Fprintf(w, "PROXY %s %s %s %d %d\r\n", protocol, src.Addr(), dst.Addr(), src.Port(), dst.Port())
	return err
}
