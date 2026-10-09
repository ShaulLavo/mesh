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
			bridge(client, backend.Addr().String(), backend.Addr().String())
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
	tlsClient := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "apps.mesh.test"})
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

func TestGatewayRejectsUnverifiedAndNonTailnetIngressBeforeBackendDial(t *testing.T) {
	for name, header := range map[string]string{
		"missing header":        "",
		"unknown source":        "PROXY UNKNOWN\r\n",
		"public source":         "PROXY TCP4 203.0.113.2 127.0.0.1 40000 443\r\n",
		"loopback source":       "PROXY TCP4 127.0.0.2 127.0.0.1 40000 443\r\n",
		"unverified connection": "direct",
	} {
		t.Run(name, func(t *testing.T) { checkRejectedGatewayIngress(t, header) })
	}
}

func checkRejectedGatewayIngress(t *testing.T, header string) {
	t.Helper()
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
	var acceptor net.Listener = tailnet.ProxyListener{Listener: listener, AllowedUIDs: []uint32{proxyTestUID(t)}}
	if header == "direct" {
		acceptor = listener
		header = ""
	}
	done := make(chan error, 1)
	go func() {
		client, acceptErr := acceptor.Accept()
		if acceptErr == nil {
			bridge(client, backend.Addr().String(), backend.Addr().String())
		}
		done <- acceptErr
	}()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(250 * time.Millisecond))
	if _, err := io.WriteString(client, header); err != nil {
		t.Fatal(err)
	}
	tlsClient := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "apps.mesh.test"})
	if err := tlsClient.Handshake(); err == nil {
		t.Fatal("invalid ingress completed TLS through gateway")
	}
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway did not close invalid ingress")
	}
	_ = backend.SetDeadline(time.Now().Add(10 * time.Millisecond))
	connection, err := backend.Accept()
	if err == nil {
		_ = connection.Close()
		t.Fatal("invalid ingress dialed a backend")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("backend accept failed unexpectedly: %v", err)
	}
}
