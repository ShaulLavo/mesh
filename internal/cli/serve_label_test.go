package cli

import (
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func TestServeLabelPreservesRunningRouteDefinition(t *testing.T) {
	host := setupCommandTestHost(t)
	before := protocol.ServiceInfo{
		Name: "5173", Kind: "proxy", Target: "5173", LocalOnly: true, Isolate: true, Healthy: true,
		Listens: []protocol.ServiceListen{{Public: 3001, Upstream: 13001}, {Public: 5173, Upstream: 15173}},
		Run:     &protocol.ServiceRun{Command: "bun run dev", Cwd: "/srv/app", Env: []string{"MODE=dev"}, IdleMillis: 60000, ReadyTimeoutMillis: 60000},
	}
	before.Demand = &protocol.ServiceDemand{State: protocol.DemandRunning, SessionID: "7K3D", Connections: 2}
	host.services = []protocol.ServiceInfo{before}
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "label", ":5173", "Fregat dev", "--host", "pc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Fregat dev") {
		t.Fatalf("label output = %q", stdout)
	}
	want := before
	want.DisplayName = "Fregat dev"
	if len(host.services) != 1 || !sameServiceDefinition(host.services[0], want) {
		t.Fatalf("relabelled route = %+v, want %+v", host.services, want)
	}
	stdout, _, err = executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Fregat dev") || !strings.Contains(stdout, "http://127.0.0.1:5173/") {
		t.Fatalf("service list = %q", stdout)
	}
	row := projectDashboardService(host.services[0], true)
	if row.Name != "Fregat dev" {
		t.Fatalf("dashboard service name = %q", row.Name)
	}
}
