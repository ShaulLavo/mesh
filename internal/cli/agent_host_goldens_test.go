package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func TestAgentDecodedHostGoldens(t *testing.T) {
	state, config, remote := t.TempDir(), t.TempDir(), t.TempDir()
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	host := HostRecord{ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:1/control/ws"}
	doc := hostprofile.HostProfile{Editable: hostprofile.Editable{SchemaVersion: 1, Description: "A fixture host", Labels: map[string]string{}, Warnings: []string{}, Instructions: "Read these owner notes.\n"}, HostID: host.ID, Revision: 2, UpdatedAt: at}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "profile.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	reader := agentHostReader{state: state, config: config, caller: host.ID, operation: "hosts.inspect", full: true, dial: agentContextFixtureDial(t, host, remote, t.TempDir(), &reads), now: func() time.Time { return at }}
	result := reader.read(t.Context(), host)
	if len(result.errors) != 0 || !result.liveAccess {
		t.Fatalf("live golden adapter: %+v", result)
	}
	assertHostGolden(t, "hosts.inspect.ok", hostDetailEnvelope(reader, result, host))
	reader.operation, reader.full = "hosts.list", false
	result = reader.read(t.Context(), host)
	fleet := hostprofile.FleetSection(hostprofile.Snapshot[hostprofile.FleetDocument]{State: hostprofile.StateAbsent, Revision: new(uint64)}, true)
	list := agentapi.HostsData{ReceivedAt: at, Fleet: contextDeclared(hostprofile.ContextReply{Fleet: fleet}, agentapi.FreshnessLive, at).Fleet, Hosts: []agentapi.HostSummary{result.host}}
	assertHostGolden(t, "hosts.list.ok", agentapi.Envelope{Operation: "hosts.list", Status: agentapi.StatusOK, Data: list})
	reader.operation, reader.full = "hosts.inspect", true
	reader.dial = serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: protocol.ErrorCodeUnknownControl, Message: "unknown context"}
	})
	result = reader.read(t.Context(), host)
	if len(result.errors) != 1 || result.errors[0].Code != agentapi.ProtocolUnsupported {
		t.Fatalf("error code lost: %+v", result.errors)
	}
	assertHostGolden(t, "hosts.inspect.partial", hostDetailEnvelope(reader, result, host))
}

func hostDetailEnvelope(reader agentHostReader, result agentHostRead, host HostRecord) agentapi.Envelope {
	envelope := agentapi.Envelope{Operation: reader.operation, Status: agentapi.StatusOK, Data: agentapi.HostDetail{Host: result.host, Access: reader.access(result, host)}, Errors: result.errors}
	if len(result.errors) > 0 {
		envelope.Status = agentapi.StatusPartial
	}
	return envelope
}

func assertHostGolden(t *testing.T, name string, envelope agentapi.Envelope) {
	t.Helper()
	envelope.SchemaVersion, envelope.RequestID = 1, "01JA0000000000000000000000"
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
		if err := os.WriteFile(path, pretty.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // Fixed testdata path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("%s golden changed\n%s", name, pretty.String())
	}
	var decoded struct {
		SchemaVersion int              `json:"schemaVersion"`
		Operation     string           `json:"operation"`
		RequestID     string           `json:"requestId"`
		Status        agentapi.Status  `json:"status"`
		Data          json.RawMessage  `json:"data"`
		Errors        []agentapi.Error `json:"errors"`
	}
	decoder := json.NewDecoder(bytes.NewReader(want))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&decoded); err != io.EOF {
		t.Fatalf("multiple objects: %v", err)
	}
	checkHostGoldenData(t, decoded.Operation, decoded.Data)
}

func checkHostGoldenData(t *testing.T, operation string, data []byte) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if operation == "hosts.list" {
		var result agentapi.HostsData
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		return
	}
	var result agentapi.HostDetail
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
}
