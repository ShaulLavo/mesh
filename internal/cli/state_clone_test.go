package cli

import (
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func TestStateViewCloneDeepOwnership(t *testing.T) {
	now := time.Unix(1700000000, 0)
	exit := 2
	view := StateView{Memory: map[string]protocol.SessionMemory{"7K3D": {Bytes: 1}}, Sessions: []protocol.SessionInfo{{ID: "7K3D", Command: []string{"shell"}, LastAttachedAt: &now, ExitCode: &exit, Recovery: &recovery.Record{Lines: []string{"line"}, Command: []string{"command"}, Restart: &recovery.Command{Argv: []string{"restart"}}, Remote: &recovery.Target{HostID: "remote"}}}}, Services: []protocol.ServiceInfo{{Listens: []protocol.ServiceListen{{Public: 1}}, Run: &protocol.ServiceRun{Env: []string{"ENV=value"}}, Demand: &protocol.ServiceDemand{Unbound: []string{"port"}}}}, Metrics: &hostmetrics.Snapshot{Temperatures: []hostmetrics.ComponentTemperature{{Label: "CPU"}}, Cores: &hostmetrics.Reading[[]float64]{Value: []float64{1, 2}}}, Sections: map[string]ObservedSection{"sessions": {Observation: protocol.Observation{AgeMillis: 1}}}}
	want := view.Clone()
	cloned := view.Clone()
	cloned.Memory["7K3D"] = protocol.SessionMemory{Bytes: 9}
	cloned.Sessions[0].Command[0] = "changed"
	*cloned.Sessions[0].LastAttachedAt = now.Add(time.Hour)
	*cloned.Sessions[0].ExitCode = 9
	cloned.Sessions[0].Recovery.Lines[0] = "changed"
	cloned.Sessions[0].Recovery.Command[0] = "changed"
	cloned.Sessions[0].Recovery.Restart.Argv[0] = "changed"
	cloned.Sessions[0].Recovery.Remote.HostID = "changed"
	cloned.Services[0].Listens[0].Public = 9
	cloned.Services[0].Run.Env[0] = "changed"
	cloned.Services[0].Demand.Unbound[0] = "changed"
	cloned.Metrics.Temperatures[0].Label = "changed"
	cloned.Metrics.Cores.Value[0] = 9
	cloned.Sections["sessions"] = ObservedSection{Observation: protocol.Observation{AgeMillis: 9}}
	if !reflect.DeepEqual(view, want) {
		t.Fatal("typed view clone aliases nested data")
	}
}

func TestWireServiceClonePreservesNilAndEmptySlices(t *testing.T) {
	value := []protocol.ServiceInfo{{Listens: []protocol.ServiceListen{}, Run: &protocol.ServiceRun{Env: []string{}}, Demand: &protocol.ServiceDemand{Unbound: []string{}}}}
	cloned := cloneWireServices(value)
	if cloned[0].Listens == nil || cloned[0].Run.Env == nil || cloned[0].Demand.Unbound == nil {
		t.Fatal("typed clone collapsed non-nil empty fields")
	}
	if cloneWireServices(nil) != nil {
		t.Fatal("typed clone changed nil slice semantics")
	}
}
