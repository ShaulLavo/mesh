//go:build !linux

package tailnet

import (
	"io"
	"runtime"
	"strings"
	"testing"
)

func TestProxyOwnerAccessUnsupported(t *testing.T) {
	uids, err := ProxyForwarderUIDs()
	if err == nil || uids != nil || !strings.Contains(err.Error(), runtime.GOOS) || !strings.Contains(err.Error(), "peer socket UID") {
		t.Fatalf("unsupported platform returned %v, %v", uids, err)
	}
	listener, client := proxyClient(t, "tcp4", "127.0.0.1:0")
	proxyListener := ProxyListener{Listener: listener, AllowedUIDs: []uint32{proxyTestUID(t)}}
	server, err := proxyListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\npayload"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("unsupported platform accepted PROXY metadata")
	}
	if got := server.RemoteAddr().String(); got != client.LocalAddr().String() {
		t.Fatalf("unsupported platform exposed source %s", got)
	}
}
