package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func onDemand() Service {
	return Service{
		Name: "dev", Kind: Proxy, Target: "5173",
		Listens: []Listen{{Public: 3001, Upstream: 13001}, {Public: 5173, Upstream: 15173}},
		Demand:  &Demand{Command: " bun run dev ", Cwd: "/srv/app/", Env: []string{"PORT=15173"}},
	}
}

func TestNormalizeOnDemandRoute(t *testing.T) {
	service := onDemand()
	service.Listens[0], service.Listens[1] = service.Listens[1], service.Listens[0]
	normalized, err := Normalize(service)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Listens[0].Public != 3001 {
		t.Fatalf("listeners are not sorted: %+v", normalized.Listens)
	}
	demand := normalized.Demand
	if demand.Command != "bun run dev" || demand.Cwd != "/srv/app" || demand.Idle != DefaultIdle || demand.ReadyTimeout != DefaultReadyTimeout {
		t.Fatalf("recipe = %+v", demand)
	}
	again, err := Normalize(normalized)
	if err != nil || !again.Equal(normalized) {
		t.Fatalf("normalization is not idempotent: %+v, %v", again, err)
	}
	if got := normalized.UpstreamPort(); got != "15173" {
		t.Fatalf("tailnet path forwards to %s, want the listener's upstream", got)
	}
	if got := strings.Join(normalized.UpstreamPorts(), ","); got != "15173,13001" {
		t.Fatalf("ready ports = %s", got)
	}
	if normalized.Label() != "serve /dev" {
		t.Fatalf("label = %q", normalized.Label())
	}
	normalized.LocalOnly = true
	normalized.Name = "5173"
	if normalized.Route() != ":5173" || strings.Join(normalized.UpstreamPorts(), ",") != "13001,15173" {
		t.Fatalf("local-only route = %s, ports %v", normalized.Route(), normalized.UpstreamPorts())
	}
}

func TestNormalizeRefusesBadOnDemandRoutes(t *testing.T) {
	cases := map[string]func(*Service){
		"directory with listeners": func(s *Service) { s.Kind = Static; s.Target = "/srv" },
		"public on-demand":         func(s *Service) { s.PublicName = "dev.shaulavo.dev" },
		"duplicate listener":       func(s *Service) { s.Listens[1].Public = 3001 },
		"listener is an upstream":  func(s *Service) { s.Listens[1].Public = 13001 },
		"zero port":                func(s *Service) { s.Listens[0].Upstream = 0 },
		"empty command":            func(s *Service) { s.Demand.Command = "  " },
		"relative cwd":             func(s *Service) { s.Demand.Cwd = "app" },
		"bad env name":             func(s *Service) { s.Demand.Env = []string{"1X=y"} },
		"env without value":        func(s *Service) { s.Demand.Env = []string{"PORT"} },
		"idle too short":           func(s *Service) { s.Demand.Idle = time.Millisecond },
		"local-only without port":  func(s *Service) { s.LocalOnly = true; s.Listens = nil },
		"local-only off its ports": func(s *Service) { s.LocalOnly = true; s.Name, s.Target = "3000", "3000" },
		"local-only name mismatch": func(s *Service) { s.LocalOnly = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			service := onDemand()
			service.Listens = append([]Listen(nil), service.Listens...)
			mutate(&service)
			if _, err := Normalize(service); err == nil {
				t.Fatal("Normalize accepted it")
			}
		})
	}
}

type fakeGate struct {
	err     error
	entered []string
	left    int
}

func (g *fakeGate) Enter(_ context.Context, name string) (func(), error) {
	g.entered = append(g.entered, name)
	if g.err != nil {
		return nil, g.err
	}
	return func() { g.left++ }, nil
}

func TestRegistryGatesOnDemandRoutesAndSkipsLocalOnlyOnes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	local := onDemand()
	local.Name, local.Target, local.LocalOnly = "4173", "4173", true
	local.Listens = []Listen{{Public: 4173, Upstream: 14173}}
	if _, err := NewRegistry([]Service{onDemand(), {Name: "api", Kind: Proxy, Target: "80", Listens: []Listen{{Public: 3001, Upstream: 9}}}}); err == nil {
		t.Fatal("two routes were allowed to listen on one port")
	}
	registry, err := NewRegistry([]Service{onDemand(), local})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/dev/", nil)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("before a gate is set: %d", response.Code)
	}

	gate := &fakeGate{err: errString("route /dev did not start: boom")}
	registry.SetDemandGate(gate)
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "boom") {
		t.Fatalf("failed start answered %d %q", response.Code, response.Body.String())
	}

	gate.err = nil
	registry.ServeHTTP(httptest.NewRecorder(), request)
	if gate.left != 1 {
		t.Fatalf("an admitted request was released %d times, want 1", gate.left)
	}
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/4173/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("a local-only route answered on the tailnet path: %d", response.Code)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
