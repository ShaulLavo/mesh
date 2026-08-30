package bootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestVerifyWebSocketReadsIdentityFromDaemon(t *testing.T) {
	t.Parallel()

	hostID := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mesh" {
			http.NotFound(w, r)
			return
		}
		_ = transport.Serve(w, r, func(_ context.Context, conn transport.Conn) error {
			frame, err := conn.ReadFrame()
			if err != nil {
				return err
			}
			request, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				return err
			}
			payload, err := (protocol.Control{
				Type:      protocol.TypeHostInfoResult,
				RequestID: request.RequestID,
				Host: &protocol.HostInfo{
					ID:            hostID,
					MeshIdentity:  hostID,
					TailscaleName: "pc.tail.example",
				},
			}).Encode()
			if err != nil {
				return err
			}
			return conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
		})
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatal(err)
	}

	host, endpoint, err := verifyWebSocket(context.Background(), []string{"127.0.0.1"}, uint16(port), "/mesh")
	if err != nil {
		t.Fatalf("verifyWebSocket() error = %v", err)
	}
	if host.ID != hostID || host.TailscaleName != "pc.tail.example" || endpoint != fmt.Sprintf("ws://127.0.0.1:%d/mesh", port) {
		t.Fatalf("host = %#v, endpoint = %q", host, endpoint)
	}
}

func TestVerifyWebSocketNamesBlockedPort(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err = verifyWebSocket(ctx, []string{"127.0.0.1"}, uint16(port), "/mesh") //nolint:gosec // net.TCPAddr ports are bounded to uint16
	assertDiagnosticCode(t, err, DiagnosticPortBlocked)
}
