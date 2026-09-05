package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type inspectResponseConn struct {
	host       HostRecord
	inspection func(protocol.Control) protocol.Control
	responses  chan protocol.Frame
}

func newInspectResponseConn(host HostRecord, inspection func(protocol.Control) protocol.Control) *inspectResponseConn {
	return &inspectResponseConn{host: host, inspection: inspection, responses: make(chan protocol.Frame, 2)}
}

func (c *inspectResponseConn) ReadFrame() (protocol.Frame, error) {
	response, ok := <-c.responses
	if !ok {
		return protocol.Frame{}, io.EOF
	}
	return response, nil
}

func (c *inspectResponseConn) WriteFrame(frame protocol.Frame) error {
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	var response protocol.Control
	switch request.Type {
	case protocol.TypeHostInfo:
		response = protocol.Control{
			Type: protocol.TypeHostInfoResult,
			Host: &protocol.HostInfo{ID: c.host.ID, MeshIdentity: c.host.MeshIdentity},
		}
	case protocol.TypeInspect:
		response = c.inspection(request)
	default:
		response = protocol.Control{Type: protocol.TypeError, Message: "unexpected request"}
	}
	response.RequestID = request.RequestID
	c.responses <- mustCommandControlFrame(response)
	return nil
}

func (c *inspectResponseConn) Close() error { return nil }

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

func TestInspectRemoteSessionRejectsInvalidResponses(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
	valid := func(preview ...string) *protocol.SessionInspection {
		return &protocol.SessionInspection{ObservedAt: observedAt, Preview: preview}
	}
	tests := []struct {
		name       string
		cols       int
		rows       int
		response   protocol.Control
		wantDetail string
	}{
		{
			name:       "wrong session",
			cols:       8,
			rows:       2,
			response:   protocol.Control{Type: protocol.TypeInspected, SessionID: "ABCD", Inspection: valid()},
			wantDetail: "different session",
		},
		{
			name:       "missing inspection",
			cols:       8,
			rows:       2,
			response:   protocol.Control{Type: protocol.TypeInspected, SessionID: "7K3D"},
			wantDetail: "no inspection",
		},
		{
			name:       "invalid inspection",
			cols:       8,
			rows:       2,
			response:   protocol.Control{Type: protocol.TypeInspected, SessionID: "7K3D", Inspection: &protocol.SessionInspection{}},
			wantDetail: "invalid observation time",
		},
		{
			name:       "protocol row limit",
			cols:       8,
			rows:       protocol.MaxInspectionPreviewRows,
			response:   protocol.Control{Type: protocol.TypeInspected, SessionID: "7K3D", Inspection: valid(make([]string, protocol.MaxInspectionPreviewRows+1)...)},
			wantDetail: "25 rows",
		},
		{
			name:       "requested row limit",
			cols:       8,
			rows:       2,
			response:   protocol.Control{Type: protocol.TypeInspected, SessionID: "7K3D", Inspection: valid("one", "two", "three")},
			wantDetail: "want at most 2",
		},
		{
			name:       "requested column limit",
			cols:       4,
			rows:       2,
			response:   protocol.Control{Type: protocol.TypeInspected, SessionID: "7K3D", Inspection: valid("12345")},
			wantDetail: "want at most 4",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := HostRecord{Alias: "pc", ID: "host-id", MeshIdentity: "host-key"}
			conn := newInspectResponseConn(host, func(protocol.Control) protocol.Control { return test.response })
			_, err := inspectRemoteSession(
				context.Background(),
				host,
				func(context.Context, HostRecord) (transport.Conn, error) { return conn, nil },
				"7K3D",
				test.cols,
				test.rows,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("inspectRemoteSession error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestInspectRemoteSessionValidatesRequestBeforeDial(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "host-id", MeshIdentity: "host-key"}
	dialed := false
	_, err := inspectRemoteSession(
		context.Background(),
		host,
		func(context.Context, HostRecord) (transport.Conn, error) {
			dialed = true
			return nil, nil
		},
		"7K3D",
		protocol.MaxInspectionPreviewCols+1,
		1,
	)
	if err == nil || !strings.Contains(err.Error(), "exceed the limit") {
		t.Fatalf("inspectRemoteSession error = %v", err)
	}
	if dialed {
		t.Fatal("invalid inspection request dialed the host")
	}
}

func TestInspectionFromProtocolDeepCopiesStyledPreview(t *testing.T) {
	source := protocol.SessionInspection{
		Preview: []string{"styled"},
		StyledPreview: []protocol.PreviewLine{{Runs: []protocol.PreviewRun{{
			Text:  "styled",
			Style: protocol.PreviewStyle{Foreground: protocol.PreviewColor{Kind: protocol.PreviewColorBasic, Value: 2}},
		}}}},
	}
	converted := inspectionFromProtocol(source)
	source.Preview[0] = "mutated"
	source.StyledPreview[0].Runs[0].Text = "mutated"
	if converted.Preview[0] != "styled" || converted.StyledPreview[0].Runs[0].Text != "styled" {
		t.Fatalf("converted inspection aliases protocol storage: %#v", converted)
	}
}
