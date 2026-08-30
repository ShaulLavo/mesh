package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestValidateHostInfoRejectsAChangedIdentity(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "expected-id", MeshIdentity: "expected-key"}
	err := validateHostInfo(host, protocol.HostInfo{ID: "other-id", MeshIdentity: "other-key"})
	if err == nil || !strings.Contains(err.Error(), "identity") || !strings.Contains(err.Error(), "pc") {
		t.Fatalf("validateHostInfo error = %v, want named identity error", err)
	}
}

func TestListRemoteHostUsesWebSocketAndVerifiesIdentity(t *testing.T) {
	createdAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(output http.ResponseWriter, request *http.Request) {
		serverErr <- transport.Serve(output, request, func(_ context.Context, conn transport.Conn) error {
			frame, err := conn.ReadFrame()
			if err != nil {
				return err
			}
			identityRequest, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				return err
			}
			if err := conn.WriteFrame(mustCommandControlFrame(protocol.Control{
				Type: protocol.TypeHostInfoResult, RequestID: identityRequest.RequestID,
				Host: &protocol.HostInfo{ID: "host-id", MeshIdentity: "host-key"},
			})); err != nil {
				return err
			}
			frame, err = conn.ReadFrame()
			if err != nil {
				return err
			}
			listRequest, err := protocol.DecodeControl(frame.Payload)
			if err != nil {
				return err
			}
			return conn.WriteFrame(mustCommandControlFrame(protocol.Control{
				Type: protocol.TypeListed, RequestID: listRequest.RequestID,
				Sessions: []protocol.SessionInfo{{
					ID: "7K3D", HostID: "host-id", Command: []string{"bash"}, State: "running", CreatedAt: createdAt,
				}},
			}))
		})
	}))
	defer server.Close()

	host := HostRecord{Alias: "pc", ID: "host-id", MeshIdentity: "host-key", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := listRemoteHost(ctx, host, dialHost)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "7K3D" || !slices.Equal(got[0].Command, []string{"bash"}) {
		t.Fatalf("remote sessions = %#v", got)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
