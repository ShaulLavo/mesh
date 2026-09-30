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
	"strings"
	"time"

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
	if host == "apps.shaulavo.dev" {
		return true
	}
	label, ok := strings.CutSuffix(host, ".shaulavo.dev")
	if !ok || len(label) != 4 {
		return false
	}
	return strings.Trim(label, "0123456789abcdefghjkmnpqrstvwxyz") == ""
}

func bridge(client net.Conn, privateAddress, appAddress string, ownerAccess bool) {
	defer func() { _ = client.Close() }()
	if ownerAccess {
		client = tailnet.NewProxyConn(client)
		_ = client.RemoteAddr()
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
	if appHost(host) {
		target = appAddress
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	_ = upstream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if ownerAccess && appHost(host) {
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
	ownerAccess := flag.Bool("tailnet-owner-access", false, "require Tailscale Serve PROXY v1 and forward device addresses to the app edge")
	flag.Parse()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		client, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go bridge(client, *privateAddress, *appAddress, *ownerAccess)
	}
}
