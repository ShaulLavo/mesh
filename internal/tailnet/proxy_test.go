package tailnet

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestProxyConnPreservesPayloadAndRemoteAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	proxy := &proxyConn{Conn: server, allowedUIDs: []uint32{42}, peerUID: peerUIDFunc(func(net.Conn) (uint32, error) { return 42, nil })}
	go func() {
		_, _ = io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 12345 443\r\npayload")
		_ = client.Close()
	}()
	if proxy.RemoteAddr().String() != "100.64.0.2:12345" {
		t.Fatalf("lost source address: %s", proxy.RemoteAddr())
	}
	body, err := io.ReadAll(proxy)
	if err != nil || string(body) != "payload" {
		t.Fatalf("lost TLS payload: %q, %v", body, err)
	}
}

func TestProxyHeaderRejectsUntrustedAndMalformedInput(t *testing.T) {
	for _, header := range []string{"PROXY UNKNOWN", "PROXY TCP4 100.64.0.2 ::1 12345 443", "PROXY TCP6 ::1 ::1 0 443", "PROXY TCP4 100.64.0.2 127.0.0.1 12345 99999", "GET / HTTP/1.1"} {
		if _, err := parseProxySource(header); err == nil {
			t.Fatalf("accepted %q", header)
		}
	}
	left, right := net.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	_ = left.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := NewProxyConn(left, []uint32{42}).Read(make([]byte, 1)); err == nil {
		t.Fatal("accepted metadata from an untrusted transport")
	}
}
