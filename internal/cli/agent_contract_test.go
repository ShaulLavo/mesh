package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

func TestAgentRegistryCommandParity(t *testing.T) {
	command := NewCommand(Dependencies{})
	for _, operation := range agentapi.Operations {
		checkAgentOperation(t, command, operation)
	}
	for _, child := range command.Commands() {
		for _, name := range append([]string{child.Name()}, child.Aliases...) {
			checkAgentNameReserved(t, name)
		}
	}
}

func checkAgentNameReserved(t *testing.T, name string) {
	t.Helper()
	if _, err := machinename.NormalizeNew(name); err == nil {
		t.Errorf("command %q admits a new machine name", name)
	}
}

func checkAgentOperation(t *testing.T, command *Command, operation agentapi.Operation) {
	t.Helper()
	words := strings.Fields(strings.TrimPrefix(operation.Command, "mesh "))
	found, rest, err := command.Find(words)
	if !operation.Implemented {
		if err == nil && len(rest) == 0 {
			t.Errorf("future operation registered: %s", operation.Name)
		}
		return
	}
	if err != nil || len(rest) != 0 {
		t.Fatalf("missing operation %s: %v", operation.Name, err)
	}
	for _, flag := range operation.Flags {
		if found.Flags().Lookup(strings.TrimPrefix(flag.Name, "--")) == nil {
			t.Errorf("%s missing %s", operation.Name, flag.Name)
		}
	}
	if got := agentapi.Classify(strings.Fields(operation.Example)[1:]); got != operation.Name {
		t.Errorf("example misclassified: %s", operation.Example)
	}
}

func TestAgentDecodedServiceGoldens(t *testing.T) {
	host := HostRecord{ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:9999/control/ws"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	snapshot, err := listAgentServices(ctx, host, serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
		if request.Type != protocol.TypeServiceList {
			t.Errorf("unexpected control %s", request.Type)
		}
		return protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, rejection := listAgentServices(ctx, host, serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: protocol.ErrorCodeUnknownControl, Message: "unsupported control"}
	}))
	var typed *ControlError
	if !errors.As(rejection, &typed) || typed.Code != protocol.ErrorCodeUnknownControl {
		t.Fatalf("control code lost: %v", rejection)
	}
	rows := liveServiceCatalogRows(host, snapshot)
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	assertAgentGolden(t, "services.ok", servicesEnvelope(rows, nil, []HostRecord{host}, at))
	cache := &serviceCatalogTestCache{rows: map[string][]storage.CachedService{host.ID: {{HostID: storage.HostID(host.ID), Service: protocol.ServiceFromInfo(snapshot.Services[0]), ObservedAt: at.Add(-time.Minute)}}}}
	cached, diagnostics, err := CollectServiceCatalog(ctx, []HostRecord{host}, time.Second, func(context.Context, HostRecord) (remoteServiceSnapshot, error) {
		return remoteServiceSnapshot{}, rejection
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentGolden(t, "services.partial", servicesEnvelope(cached, diagnostics, []HostRecord{host}, at))
	assertAgentGolden(t, "services.unsupported", servicesEnvelope(nil, map[string]error{host.ID: rejection}, []HostRecord{host}, at))
	stdout, stderr, usageErr := executeCommand(t, Dependencies{}, "serve", "ls", "--json", "--bogus")
	assertAgentFailure(t, stdout, stderr, usageErr, "usage.invalid", 2)
	var usage agentapi.Envelope
	if err := json.Unmarshal([]byte(stdout), &usage); err != nil {
		t.Fatal(err)
	}
	assertAgentGolden(t, "services.usage", usage)
}

func assertAgentGolden(t *testing.T, name string, envelope agentapi.Envelope) {
	t.Helper()
	envelope.SchemaVersion, envelope.Operation, envelope.RequestID = 1, "services.list", "01JA0000000000000000000000"
	var output bytes.Buffer
	if err := agentapi.Write(&output, envelope); err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, output.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "agentapi", "testdata", name+".json")
	if os.Getenv("MESH_UPDATE_AGENT_GOLDENS") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		} //nolint:gosec // explicit test golden update
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed testdata golden path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("%s golden changed\n%s", name, pretty.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(want))
	decoder.DisallowUnknownFields()
	var checked agentapi.Envelope
	if err := decoder.Decode(&checked); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&checked); err != io.EOF {
		t.Fatal("multiple envelopes")
	}
	if envelope.Data != nil {
		checkAgentServiceDataGolden(t, checked.Data)
	}
}

func checkAgentServiceDataGolden(t *testing.T, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var checked agentapi.ServicesData
	if err := decoder.Decode(&checked); err != nil {
		t.Fatal(err)
	}
	if checked.Services == nil || checked.HostErrors == nil {
		t.Fatal("null result arrays")
	}
}

func TestAgentTypedControlErrorsKeepHumanRendering(t *testing.T) {
	for _, code := range []string{protocol.ErrorCodeUnknownControl, "session.not_found", "unrecognized.code"} {
		response := protocol.Control{Type: protocol.TypeError, ErrorCode: code, Message: "refused\n\u202e"}
		encoded, err := response.Encode()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := protocol.DecodeControl(encoded)
		if err != nil {
			t.Fatal(err)
		}
		failure := daemonResponseError("session inspection", decoded.Message, decoded.ErrorCode)
		var typed *ControlError
		if !errors.As(failure, &typed) || typed.Code != code || failure.Error() != "cli: daemon rejected session inspection: refused" {
			t.Fatalf("lost typed/human error: %v", failure)
		}
	}
}

type agentBrokenWriter struct{ writes int }

func (w *agentBrokenWriter) Write([]byte) (int, error) { w.writes++; return 0, io.ErrClosedPipe }

func TestAgentOutputAttemptNeverRetries(t *testing.T) {
	invocation := agentInvocation{operation: "services.list"}
	writer := &agentBrokenWriter{}
	err := invocation.write(writer, agentapi.Envelope{Status: agentapi.StatusOK})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := invocation.failure(writer, err); !errors.Is(err, io.ErrClosedPipe) || writer.writes != 1 {
		t.Fatalf("second envelope write: %d %v", writer.writes, err)
	}
}
