package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestGatewayRejectsDisallowedForwarderBeforeBackendDial(t *testing.T) {
	backend, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	proxyListener := tailnet.ProxyListener{Listener: listener, AllowedUIDs: []uint32{proxyTestUID(t) + 1}}
	done := make(chan error, 1)
	go func() {
		client, err := proxyListener.Accept()
		if err == nil {
			bridge(client, backend.Addr().String(), backend.Addr().String(), true)
		}
		done <- err
	}()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\n"); err != nil {
		t.Fatal(err)
	}
	tlsClient := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "apps.shaulavo.dev"})
	if err := tlsClient.Handshake(); err == nil {
		t.Fatal("disallowed forwarder completed TLS through gateway")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway did not close rejected forwarder")
	}
	_ = backend.SetDeadline(time.Now().Add(10 * time.Millisecond))
	connection, err := backend.Accept()
	if err == nil {
		_ = connection.Close()
		t.Fatal("gateway dialed a backend for a disallowed forwarder")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("backend accept failed unexpectedly: %v", err)
	}
}
