// The Tailnet gateway routes encrypted connections by SNI. Mesh terminates TLS
// on each backend; no certificate keys or application credentials enter here.
package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/tailnet"
)

type helloConn struct {
	net.Conn
	reader io.Reader
}

func (c helloConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// The parser stops at ClientHello. Suppress its rejection alert because the
// untouched handshake will be replayed to the actual TLS server.
func (c helloConn) Write(p []byte) (int, error) { return len(p), nil }

func appHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	label, _, ok := domainpolicy.Label(host, false)
	if !ok {
		return false
	}
	if label == "apps" {
		return true
	}
	if len(label) != 4 {
		return false
	}
	return strings.Trim(label, "0123456789abcdefghjkmnpqrstvwxyz") == ""
}

func verifiedClient(client net.Conn) bool {
	verified, ok := client.(interface{ Authenticated() bool })
	if !ok || !verified.Authenticated() {
		return false
	}
	source, err := netip.ParseAddrPort(client.RemoteAddr().String())
	return err == nil && !source.Addr().IsLoopback() && !source.Addr().IsUnspecified()
}

func bridge(client net.Conn, privateAddress, appAddress string, ownerAccess bool) {
	defer func() { _ = client.Close() }()
	if ownerAccess && !verifiedClient(client) {
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	var captured bytes.Buffer
	var host string
	probe := helloConn{Conn: client, reader: io.TeeReader(io.LimitReader(client, 65536), &captured)}
	parser := tls.Server(probe, &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		host = strings.ToLower(hello.ServerName)
		return nil, errors.New("ClientHello captured")
	}})
	_ = parser.Handshake()
	if host == "" {
		return
	}
	target := privateAddress
	if !appHost(host) && !ownerAccess {
		return
	}
	if appHost(host) {
		target = appAddress
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	_ = upstream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if ownerAccess {
		if err := tailnet.WriteProxyHeader(upstream, client.RemoteAddr(), upstream.RemoteAddr()); err != nil {
			return
		}
	}
	if _, err := io.Copy(upstream, &captured); err != nil {
		return
	}
	_ = upstream.SetWriteDeadline(time.Time{})
	_ = client.SetReadDeadline(time.Time{})
	go func() {
		_, _ = io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, upstream)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8446", "loopback listener for Tailscale TCP/443")
	privateAddress := flag.String("private", "127.0.0.1:8443", "existing private Mesh TLS listener")
	appAddress := flag.String("apps", "127.0.0.1:8445", "temporary-app edge TLS listener")
	ownerAccess := flag.Bool("tailnet-owner-access", false, "require Tailscale Serve PROXY v1 and forward device addresses to both TLS backends")
	domains := flag.String("domains", "", "deployment domain policy JSON file")
	flag.Parse()
	if *domains == "" {
		log.Fatal("deployment domain policy is required")
	}
	if err := domainpolicy.Initialize(*domains); err != nil {
		log.Fatal(err)
	}
	if domainpolicy.Primary() == "" {
		log.Fatal("deployment domain policy must contain a primary domain")
	}
	var allowedUIDs []uint32
	if *ownerAccess {
		var err error
		allowedUIDs, err = tailnet.ProxyForwarderUIDs()
		if err != nil {
			log.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if *ownerAccess {
		listener = tailnet.ProxyListener{Listener: listener, AllowedUIDs: allowedUIDs}
	}
	for {
		client, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go bridge(client, *privateAddress, *appAddress, *ownerAccess)
	}
}
