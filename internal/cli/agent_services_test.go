package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestAgentServicesAuthenticatedLiveReadPreservesMissingCache(t *testing.T) {
	state := t.TempDir()
	config := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	auth, id := controlFixtureAuthentication(t)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
			return agentServiceFixture(ctx, conn, id, &reads)
		})
	}))
	t.Cleanup(server.Close)
	host := HostRecord{ID: id, MeshIdentity: id, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeCommand(t, Dependencies{}, "serve", "list", "--json")
	if err != nil || stderr != "" {
		t.Fatalf("live read: %v %q", err, stderr)
	}
	var envelope struct {
		Data   agentapi.ServicesData
		Status string
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "ok" || len(envelope.Data.Services) != 1 || envelope.Data.Services[0].Freshness != agentapi.FreshnessLive || envelope.Data.Services[0].ObservedAt == nil {
		t.Fatal(stdout)
	}
	if reads.Load() != 2 {
		t.Fatalf("expected host.info and service.list, got %d", reads.Load())
	}
	assertAgentAbsent(t, filepath.Join(state, "s"), filepath.Join(state, catalogDatabaseName), filepath.Join(config, "machine-names"))
}

func agentServiceFixture(ctx context.Context, conn transport.Conn, id string, reads *atomic.Int32) error {
	for ctx.Err() == nil {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("fixture read: %w", err)
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("fixture control: %w", err)
		}
		response, err := agentServiceFixtureResponse(request, id)
		if err != nil {
			return err
		}
		reads.Add(1)
		if err := conn.WriteFrame(mustCommandControlFrame(response)); err != nil {
			return fmt.Errorf("fixture write: %w", err)
		}
	}
	return nil
}

func agentServiceFixtureResponse(request protocol.Control, id string) (protocol.Control, error) {
	response := protocol.Control{RequestID: request.RequestID}
	switch request.Type {
	case protocol.TypeHostInfo:
		response.Type, response.Host = protocol.TypeHostInfoResult, &protocol.HostInfo{ID: id, MeshIdentity: id, MachineName: "destination", NameRevision: 1, ServiceHealthSupported: true}
	case protocol.TypeServiceList:
		response.Type, response.Services = protocol.TypeServiceListed, []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}
	default:
		return protocol.Control{}, fmt.Errorf("unexpected passive control: %s", request.Type)
	}
	return response, nil
}

func TestAgentServiceTimeoutKeepsSuccessfulEmptyHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(root, "config"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(root, "state"))
	ids := argumentIdentities(t)
	hosts := []HostRecord{{ID: ids[0], MeshIdentity: ids[0], Endpoint: "ws://127.0.0.1:1/control/ws"}, {ID: ids[1], MeshIdentity: ids[1], Endpoint: "ws://127.0.0.1:1/control/ws"}}
	for _, host := range hosts {
		if err := SaveHost(host); err != nil {
			t.Fatal(err)
		}
	}
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		if host.ID == ids[1] {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return serviceRemoteDial(host, func(protocol.Control) protocol.Control { return protocol.Control{Type: protocol.TypeServiceListed} })(ctx, host)
	}
	stdout, stderr, err := executeCommand(t, Dependencies{DialPassive: dial}, "serve", "ls", "--json", "--timeout=30ms")
	status, ok := StatusCode(err)
	if !ok || status != 3 || stderr != "" {
		t.Fatalf("partial timeout: %v %q %q", err, stdout, stderr)
	}
	var envelope struct {
		Status string
		Data   agentapi.ServicesData
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "partial" || envelope.Data.Services == nil || len(envelope.Data.HostErrors) != 1 {
		t.Fatal(stdout)
	}
	assertAgentAbsent(t, filepath.Join(root, "state"))
}

func TestAgentMalformedIdentityIsPreserved(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	key := filepath.Join(state, "identity.key")
	if err := os.WriteFile(key, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := dialAgentHost(t.Context(), HostRecord{})
	var failure *agentapi.Failure
	if !errors.As(err, &failure) || failure.Err.Code != agentapi.StateUnavailable {
		t.Fatalf("invalid identity: %v", err)
	}
	bytes, err := os.ReadFile(key) //nolint:gosec // test-owned malformed key
	if err != nil || string(bytes) != "invalid" {
		t.Fatal("malformed key replaced")
	}
}

func TestAgentServicesCacheFailurePreservesLiveResult(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{ID: argumentIdentities(t)[1], MeshIdentity: argumentIdentities(t)[1], Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, catalogDatabaseName), []byte("damaged cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	dial := serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}}
	})
	stdout, _, err := executeCommand(t, Dependencies{DialPassive: dial}, "serve", "ls", "--json")
	if status, ok := StatusCode(err); !ok || status != 3 {
		t.Fatalf("cache error discarded live data: %v %s", err, stdout)
	}
	var envelope struct {
		Data   agentapi.ServicesData
		Errors []agentapi.Error
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Services) != 1 || len(envelope.Errors) != 1 || envelope.Errors[0].Code != agentapi.StateUnavailable {
		t.Fatalf("wrong cache partial result: %s", stdout)
	}
}
