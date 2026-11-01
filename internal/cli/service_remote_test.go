package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type serviceRemoteTestConn struct {
	mu        sync.Mutex
	handler   func(protocol.Control) protocol.Control
	responses chan protocol.Frame
}

func newServiceRemoteTestConn(handler func(protocol.Control) protocol.Control) *serviceRemoteTestConn {
	return &serviceRemoteTestConn{handler: handler, responses: make(chan protocol.Frame, 8)}
}

func (c *serviceRemoteTestConn) WriteFrame(frame protocol.Frame) error {
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	response := c.handler(request)
	c.mu.Unlock()
	response.RequestID = request.RequestID
	payload, err := response.Encode()
	if err != nil {
		return err
	}
	c.responses <- protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	return nil
}

func (c *serviceRemoteTestConn) ReadFrame() (protocol.Frame, error) {
	response, ok := <-c.responses
	if !ok {
		return protocol.Frame{}, io.EOF
	}
	return response, nil
}

func (c *serviceRemoteTestConn) Close() error { return nil }

func serviceRemoteDial(host HostRecord, handler func(protocol.Control) protocol.Control) HostDialer {
	return func(context.Context, HostRecord) (transport.Conn, error) {
		return newServiceRemoteTestConn(func(request protocol.Control) protocol.Control {
			if request.Type == protocol.TypeHostInfo {
				info := testHostDeclaration(host)
				info.ServiceHealthSupported = true
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: info}
			}
			return handler(request)
		}), nil
	}
}

func TestRemoteServiceBoundaryRejectsChangedPreviewAndAcknowledgement(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"}
	requested := protocol.ServiceInfo{Name: "blog", Target: "./site"}
	_, _, err := previewRemoteService(context.Background(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeServicePreviewed, ServicePreview: &protocol.ServicePreview{
			Service: protocol.ServiceInfo{Name: "other", Kind: "static", Target: "/home/me/site"},
		}}
	}), requested)
	if err == nil || !strings.Contains(err.Error(), "changed service semantics") {
		t.Fatalf("changed preview error = %v", err)
	}

	preview := protocol.ServicePreview{Service: protocol.ServiceInfo{Name: "blog", Kind: "static", Target: "/home/me/site"}}
	_, err = upsertRemoteService(context.Background(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		ack := preview.Service
		ack.Target = "/home/me/other"
		return protocol.Control{Type: protocol.TypeServiceUpserted, Service: &ack}
	}), requested, preview, "")
	if err == nil || !strings.Contains(err.Error(), "different service definition") {
		t.Fatalf("changed acknowledgement error = %v", err)
	}
}

func TestRemoteServiceBoundaryPinsInferredKindAndCanonicalPort(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"}
	for _, test := range []struct {
		name      string
		requested protocol.ServiceInfo
		returned  protocol.ServiceInfo
	}{
		{name: "numeric kind", requested: protocol.ServiceInfo{Name: "api", Target: "03000"}, returned: protocol.ServiceInfo{Name: "api", Kind: "static", Target: "/srv"}},
		{name: "numeric port", requested: protocol.ServiceInfo{Name: "api", Target: "03000"}, returned: protocol.ServiceInfo{Name: "api", Kind: "proxy", Target: "3001"}},
		{name: "directory kind", requested: protocol.ServiceInfo{Name: "site", Target: "./site"}, returned: protocol.ServiceInfo{Name: "site", Kind: "proxy", Target: "3000"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := previewRemoteService(context.Background(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeServicePreviewed, ServicePreview: &protocol.ServicePreview{Service: test.returned}}
			}), test.requested)
			if err == nil || !strings.Contains(err.Error(), "invalid service preview") {
				t.Fatalf("malicious inference error = %v", err)
			}
		})
	}
}

func TestRemoteServiceBoundaryDoesNotEchoOversizedInvalidFields(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"}
	marker := strings.Repeat("ATTACKER\r\n", 50_000)
	_, _, err := previewRemoteService(context.Background(), host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeServicePreviewed, ServicePreview: &protocol.ServicePreview{
			Service: protocol.ServiceInfo{Name: "site", Kind: marker, Target: "/srv/site"},
		}}
	}), protocol.ServiceInfo{Name: "site", Target: "./site"})
	if err == nil || strings.Contains(err.Error(), "ATTACKER") || len(err.Error()) > 1024 {
		t.Fatalf("oversized invalid service error = %q (%d bytes)", err, len(err.Error()))
	}
}

func TestRemoteServiceTransportErrorsAreBoundedAndPreserveCause(t *testing.T) {
	host := HostRecord{MachineName: "pc", ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"}
	cause := errors.New("ATTACKER\r\n\u202e" + strings.Repeat("x", 10_000))
	for _, test := range []struct {
		name string
		dial HostDialer
	}{
		{name: "dial", dial: func(context.Context, HostRecord) (transport.Conn, error) { return nil, cause }},
		{name: "read", dial: func(context.Context, HostRecord) (transport.Conn, error) {
			return &failingCLIConn{readErr: fmt.Errorf("websocket close: %w", cause)}, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := listRemoteServices(context.Background(), host, test.dial)
			if err == nil || !errors.Is(err, cause) || strings.ContainsAny(err.Error(), "\r\n\x1b") || strings.ContainsRune(err.Error(), '\u202e') || len(err.Error()) > maximumRemoteErrorBytes+150 {
				t.Fatalf("bounded %s error = %q (%d bytes), errors.Is = %v", test.name, err, len(err.Error()), errors.Is(err, cause))
			}
		})
	}
}

type failingCLIConn struct {
	writeErr error
	readErr  error
}

func (c *failingCLIConn) WriteFrame(protocol.Frame) error { return c.writeErr }
func (c *failingCLIConn) ReadFrame() (protocol.Frame, error) {
	return protocol.Frame{}, c.readErr
}
func (*failingCLIConn) Close() error { return nil }

func TestRemoteServiceErrorTextIsBoundedAndSingleLine(t *testing.T) {
	host := HostRecord{MachineName: "pc"}
	response := protocol.Control{Type: protocol.TypeError, Message: "ATTACKER\r\n\u202e" + strings.Repeat("x", 2000)}
	err := remoteServiceResponseError(host, "service preview", response)
	if strings.ContainsAny(err.Error(), "\r\n") || strings.ContainsRune(err.Error(), '\u202e') || len(err.Error()) > maximumRemoteErrorBytes+100 || !strings.Contains(err.Error(), "ATTACKER") {
		t.Fatalf("sanitized error = %q (%d bytes)", err, len(err.Error()))
	}
}

// Synthetic legacy fixtures have no owner declaration; canonical fixtures publish one.
func testHostDeclaration(host HostRecord) *protocol.HostInfo {
	info := &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity}
	if machinename.ValidateClaim(host.ID, machinename.Claim{ID: host.ID, MachineName: host.MachineName, Revision: 1}) == nil && host.ID == host.MeshIdentity {
		info.MachineName, info.NameRevision = host.MachineName, 1
	}
	return info
}
