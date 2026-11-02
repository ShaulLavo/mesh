package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/wake"
)

func TestStateCloneDeepOwnership(t *testing.T) {
	now := time.Unix(1700000000, 0)
	exit := 2
	host := protocol.HostInfo{ID: "host", Build: &release.Build{Version: "1"}, Wake: &wake.Grant{TargetID: "target", NIC: &wake.NIC{Address: "192.0.2.1"}, Signature: []byte{1, 2}}}
	session := protocol.SessionInfo{ID: "7K3D", Command: []string{"shell"}, LastAttachedAt: &now, ExitCode: &exit, Recovery: &recovery.Record{Agent: &agentresume.Recipe{Launch: agentresume.Launch{Options: []string{"option"}}}, AgentResume: &agentresume.Receipt{ConversationID: "conversation"}, Lines: []string{"line"}, Command: []string{"command"}, Restart: &recovery.Command{Argv: []string{"restart"}}, Remote: &recovery.Target{HostID: "remote"}}, Hibernated: &recovery.Hibernation{Reason: "idle"}}
	service := protocol.ServiceInfo{Listens: []protocol.ServiceListen{{Public: 1}}, Run: &protocol.ServiceRun{Env: []string{"ENV=value"}}, Demand: &protocol.ServiceDemand{Unbound: []string{"port"}}}
	metrics := hostmetrics.Snapshot{Temperatures: []hostmetrics.ComponentTemperature{{Label: "CPU"}}, Battery: &hostmetrics.Reading[hostmetrics.Battery]{Value: hostmetrics.Battery{State: "charging"}}, GPU: &hostmetrics.Reading[hostmetrics.GPU]{Value: hostmetrics.GPU{MemoryKind: "shared"}}, Disk: &hostmetrics.Reading[hostmetrics.Disk]{Value: hostmetrics.Disk{BusyPercent: 1}}, Network: &hostmetrics.Reading[hostmetrics.Network]{Value: hostmetrics.Network{SendBytesPerSecond: 1}}, Cores: &hostmetrics.Reading[[]float64]{Value: []float64{1, 2}}}
	source := protocol.StateEvent{Payload: protocol.StatePayload{Host: &host, Memory: map[string]protocol.SessionMemory{"7K3D": {Bytes: 1}}, Session: &session, Service: &service, Metrics: &metrics}}
	want := cloneStateEvent(source)
	cloned := cloneStateEvent(source)
	cloned.Payload.Host.Build.Version = "changed"
	cloned.Payload.Host.Wake.NIC.Address = "198.51.100.1"
	cloned.Payload.Host.Wake.Signature[0] = 9
	cloned.Payload.Memory["7K3D"] = protocol.SessionMemory{Bytes: 9}
	cloned.Payload.Session.Command[0] = "changed"
	*cloned.Payload.Session.LastAttachedAt = now.Add(time.Hour)
	*cloned.Payload.Session.ExitCode = 9
	cloned.Payload.Session.Recovery.Agent.Options[0] = "changed"
	cloned.Payload.Session.Recovery.AgentResume.ConversationID = "changed"
	cloned.Payload.Session.Recovery.Lines[0] = "changed"
	cloned.Payload.Session.Recovery.Command[0] = "changed"
	cloned.Payload.Session.Recovery.Restart.Argv[0] = "changed"
	cloned.Payload.Session.Recovery.Remote.HostID = "changed"
	cloned.Payload.Session.Hibernated.Reason = "changed"
	cloned.Payload.Service.Listens[0].Public = 9
	cloned.Payload.Service.Run.Env[0] = "changed"
	cloned.Payload.Service.Demand.Unbound[0] = "changed"
	cloned.Payload.Metrics.Temperatures[0].Label = "changed"
	cloned.Payload.Metrics.Battery.Value.State = "changed"
	cloned.Payload.Metrics.GPU.Value.MemoryKind = "changed"
	cloned.Payload.Metrics.Disk.Value.BusyPercent = 9
	cloned.Payload.Metrics.Network.Value.SendBytesPerSecond = 9
	cloned.Payload.Metrics.Cores.Value[0] = 9
	if !reflect.DeepEqual(source, want) {
		t.Fatal("typed state clone aliases nested data")
	}
}

func TestStateClonePreservesNilAndEmptySlices(t *testing.T) {
	value := protocol.StatePayload{Memory: map[string]protocol.SessionMemory{}, Session: &protocol.SessionInfo{Command: []string{}}, Service: &protocol.ServiceInfo{Listens: []protocol.ServiceListen{}, Run: &protocol.ServiceRun{Env: []string{}}, Demand: &protocol.ServiceDemand{Unbound: []string{}}}}
	cloned := cloneStatePayload(value)
	if cloned.Memory == nil || cloned.Session.Command == nil || cloned.Service.Listens == nil || cloned.Service.Run.Env == nil || cloned.Service.Demand.Unbound == nil {
		t.Fatal("typed clone collapsed non-nil empty fields")
	}
	if cloneStatePayload(protocol.StatePayload{}).Memory != nil {
		t.Fatal("typed clone changed nil map semantics")
	}
}
