package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

func defaultDemandFlags() serveFlags {
	return serveFlags{idle: meshserve.DefaultIdle, readyTimeout: meshserve.DefaultReadyTimeout}
}

func TestServeDemandFlags(t *testing.T) {
	flags := defaultDemandFlags()
	flags.run, flags.cwd, flags.cwdSet = "bun run dev", "/srv/app", true
	flags.env = []string{"PORT=15173"}
	flags.listens = []string{"5173=15173", "3001=13001"}
	demand, err := serveDemandFromFlags("5173", flags, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(demand.listens) != 2 || demand.listens[1] != (protocol.ServiceListen{Public: 3001, Upstream: 13001}) {
		t.Fatalf("listens = %+v", demand.listens)
	}
	if demand.run.Cwd != "/srv/app" || demand.run.IdleMillis != meshserve.DefaultIdle.Milliseconds() {
		t.Fatalf("run = %+v", demand.run)
	}

	refused := map[string]func(*serveFlags) string{
		"cwd without run":  func(f *serveFlags) string { f.run, f.cwdSet = "", true; return "5173" },
		"idle without run": func(f *serveFlags) string { f.run, f.idleSet = "", true; return "5173" },
		"directory target": func(f *serveFlags) string { return "./site" },
		"public":           func(f *serveFlags) string { f.publicName = "dev.shaulavo.dev"; return "5173" },
		"bad listen":       func(f *serveFlags) string { f.listens = []string{"5173:15173"}; return "5173" },
		"self listen":      func(f *serveFlags) string { f.listens = []string{"5173=5173"}; return "5173" },
		"bad env":          func(f *serveFlags) string { f.env = []string{"NOVALUE"}; return "5173" },
		"short idle":       func(f *serveFlags) string { f.idle = time.Millisecond; return "5173" },
	}
	for name, mutate := range refused {
		t.Run(name, func(t *testing.T) {
			candidate := defaultDemandFlags()
			candidate.run = "bun run dev"
			target := mutate(&candidate)
			if _, err := serveDemandFromFlags(target, candidate, true); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestServeRouteNames(t *testing.T) {
	for route, want := range map[string]string{"/dev": "dev", ":5173": "5173"} {
		if got, err := serviceNameFromRoute(route); err != nil || got != want {
			t.Fatalf("%s → %q, %v", route, got, err)
		}
	}
	for _, route := range []string{":0", ":05173", ":70000", ":dev"} {
		if _, err := serviceNameFromRoute(route); err == nil {
			t.Fatalf("%s was accepted", route)
		}
	}
}

func TestServeListShowsDemandState(t *testing.T) {
	service := protocol.ServiceInfo{
		Name: "dev", Kind: "proxy", Target: "5173", Healthy: true,
		Listens: []protocol.ServiceListen{{Public: 3001, Upstream: 13001}, {Public: 5173, Upstream: 15173}},
		Run:     &protocol.ServiceRun{Command: "bun run dev", Cwd: "/srv"},
		Demand:  &protocol.ServiceDemand{State: protocol.DemandRunning, Connections: 3},
	}
	row := ServiceCatalogRow{Service: service, Live: true}
	if row.State() != "running (3 conns)" || row.Health() != "healthy" {
		t.Fatalf("running row: %s / %s", row.State(), row.Health())
	}
	row.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
	if row.State() != "stopped" || row.Health() != "stopped" {
		t.Fatalf("stopped row: %s / %s", row.State(), row.Health())
	}
	if got := serviceTargetCell(service); got != ":3001→13001 :5173→15173" {
		t.Fatalf("target cell = %q", got)
	}
	service.Target = "8080"
	if got := serviceTargetCell(service); !strings.HasPrefix(got, "8080 ") {
		t.Fatalf("target outside the listeners is hidden: %q", got)
	}
	if row.Scope() != "tailnet" {
		t.Fatalf("a route with a tailnet path has scope %q", row.Scope())
	}
	row.Service.LocalOnly = true
	if row.Scope() != "local" {
		t.Fatalf("a local-only route has scope %q", row.Scope())
	}
	row.Live = false
	if row.State() != "-" {
		t.Fatalf("offline row state = %q", row.State())
	}
}

func TestServeDemandCwdOnAnotherHost(t *testing.T) {
	flags := defaultDemandFlags()
	flags.run = "bun run dev"
	if _, err := serveDemandFromFlags("5173", flags, false); err == nil || !strings.Contains(err.Error(), "--cwd") {
		t.Fatalf("a remote --run without --cwd = %v, want it refused", err)
	}
	flags.cwd, flags.cwdSet = "projects/app", true
	demand, err := serveDemandFromFlags("5173", flags, false)
	if err != nil || demand.run.Cwd != "projects/app" {
		t.Fatalf("a relative remote --cwd became %+v, %v; want it sent as typed", demand.run, err)
	}
	requested := protocol.ServiceInfo{Name: "dev", Target: "5173", Run: demand.run}
	resolved := protocol.ServiceInfo{Name: "dev", Kind: "proxy", Target: "5173", Run: &protocol.ServiceRun{
		Command: "bun run dev", Cwd: "/home/pc/projects/app",
		IdleMillis: demand.run.IdleMillis, ReadyTimeoutMillis: demand.run.ReadyTimeoutMillis,
	}}
	if !sameDemandDefinition(requested, resolved) {
		t.Fatal("the host's home-relative resolution was taken for a changed recipe")
	}
	resolved.Run.Cwd = "/home/pc/other"
	if sameDemandDefinition(requested, resolved) {
		t.Fatal("a different directory was accepted")
	}
}

func TestServeRouteNaming(t *testing.T) {
	listens := []protocol.ServiceListen{{Public: 5173, Upstream: 15173}, {Public: 3001, Upstream: 13001}}
	for _, test := range []struct {
		route, target, name string
		local               bool
		fails               bool
	}{
		{route: "/dev", target: "5173", name: "dev"},
		{route: "", target: "5173", name: "5173", local: true},
		{route: ":3001", target: "3001", name: "3001", local: true},
		{route: ":3001", target: "5173", fails: true},
		{route: "", target: "3000", fails: true},
		{route: ":9999", target: "9999", fails: true},
	} {
		name, local, err := serveRouteName(test.route, test.target, listens)
		if test.fails {
			if err == nil {
				t.Fatalf("%q/%q accepted as %s", test.route, test.target, name)
			}
			continue
		}
		if err != nil || name != test.name || local != test.local {
			t.Fatalf("%q/%q = %s local=%v %v", test.route, test.target, name, local, err)
		}
	}
	if _, _, err := serveRouteName("", "5173", nil); err == nil {
		t.Fatal("a route with no --at and no --listen was accepted")
	}
}
