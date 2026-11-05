package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func existingRecipe(kept bool) protocol.ServiceInfo {
	run := &protocol.ServiceRun{Command: "./old", Cwd: "/srv/old", ReadyTimeoutMillis: 90_000, IdleMillis: 300_000}
	if kept {
		run.KeepRunning, run.IdleMillis = true, 0
	}
	return protocol.ServiceInfo{Name: "dev", Kind: "proxy", Target: "5173", Run: run, Healthy: true}
}

func persistedRecipe(t *testing.T, host *commandTestHost) *protocol.ServiceRun {
	t.Helper()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.services) != 1 || host.services[0].Run == nil {
		t.Fatalf("host services = %#v, want one recipe", host.services)
	}
	return host.services[0].Run
}

type recipeExpectation struct {
	kept  bool
	idle  int64
	ready int64
}

// Each field of an edited recipe is inherited by its own flags: lifetime by
// --idle and --keep-running, readiness by --ready-timeout. The payload the
// host received is what is checked, and the confirmation is the host's.
func TestServeRecipeInheritsEachFieldByItsOwnFlags(t *testing.T) {
	for _, existingKept := range []bool{false, true} {
		runRecipeInheritanceCases(t, existingKept)
	}
}

func runRecipeInheritanceCases(t *testing.T, existingKept bool) {
	t.Helper()
	for _, test := range []struct {
		name  string
		flags []string
		want  func(existingKept bool) recipeExpectation
	}{
		{name: "omitted", want: func(kept bool) recipeExpectation {
			if kept {
				return recipeExpectation{kept: true, ready: 90_000}
			}
			return recipeExpectation{idle: 300_000, ready: 90_000}
		}},
		{name: "idle", flags: []string{"--idle", "10m"}, want: func(bool) recipeExpectation { return recipeExpectation{idle: 600_000, ready: 90_000} }},
		{name: "keep", flags: []string{"--keep-running"}, want: func(bool) recipeExpectation { return recipeExpectation{kept: true, ready: 90_000} }},
		{name: "omitted with ready", flags: []string{"--ready-timeout", "20s"}, want: func(kept bool) recipeExpectation {
			if kept {
				return recipeExpectation{kept: true, ready: 20_000}
			}
			return recipeExpectation{idle: 300_000, ready: 20_000}
		}},
		{name: "idle with ready", flags: []string{"--idle", "10m", "--ready-timeout", "20s"}, want: func(bool) recipeExpectation { return recipeExpectation{idle: 600_000, ready: 20_000} }},
		{name: "keep with ready", flags: []string{"--keep-running", "--ready-timeout", "20s"}, want: func(bool) recipeExpectation { return recipeExpectation{kept: true, ready: 20_000} }},
	} {
		name := map[bool]string{false: "finite ", true: "kept "}[existingKept] + test.name
		t.Run(name, func(t *testing.T) {
			assertInheritedRecipe(t, existingKept, test.flags, test.want(existingKept))
		})
	}
}

func assertInheritedRecipe(t *testing.T, existingKept bool, flags []string, want recipeExpectation) {
	t.Helper()
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{existingRecipe(existingKept)}
	args := append([]string{"serve", "pc", "5173", "--at", "/dev", "--run", "bun run dev", "--cwd", "/srv/app"}, flags...)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, args...)
	if err != nil {
		t.Fatal(err)
	}
	run := persistedRecipe(t, host)
	if run.Command != "bun run dev" || run.KeepRunning != want.kept || run.IdleMillis != want.idle || run.ReadyTimeoutMillis != want.ready {
		t.Fatalf("persisted recipe = %+v, want %+v", run, want)
	}
	if want.kept != strings.Contains(stdout, "now and keeps it running") || want.kept == strings.Contains(stdout, "no counted connection") {
		t.Fatalf("confirmation = %q, want the persisted lifetime", stdout)
	}
}

func TestServeNewRecipeUsesDefaults(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "5173", "--at", "/dev", "--run", "bun run dev", "--cwd", "/srv/app")
	if err != nil {
		t.Fatal(err)
	}
	if run := persistedRecipe(t, host); run.KeepRunning || run.IdleMillis != 900_000 || run.ReadyTimeoutMillis != 60_000 {
		t.Fatalf("new recipe = %+v, want the 15m idle and 60s ready defaults", run)
	}
	for _, want := range []string{"on the first connection through Mesh", "stops after 15m0s with no counted connection", "use --keep-running for clients that use the upstream port directly"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("confirmation = %q, want %q", stdout, want)
		}
	}
}

func TestServeRefusesAmbiguousLifetimes(t *testing.T) {
	base := []string{"serve", "pc", "5173", "--at", "/dev"}
	run := []string{"--run", "bun run dev", "--cwd", "/srv/app"}
	for _, test := range []struct {
		name  string
		flags []string
		want  string
	}{
		{name: "keep false", flags: append(run, "--keep-running=false"), want: "use --idle DURATION"},
		{name: "keep and idle", flags: append(run, "--keep-running", "--idle", "5m"), want: "conflict"},
		{name: "keep without run", flags: []string{"--keep-running"}, want: "--keep-running needs --run"},
		{name: "false keep without run", flags: []string{"--keep-running=false"}, want: "--keep-running needs --run"},
		{name: "zero idle", flags: append(run, "--idle", "0"), want: "use --keep-running"},
		{name: "negative idle", flags: append(run, "--idle", "-1s"), want: "--idle must be at least 1s"},
		{name: "subsecond idle", flags: append(run, "--idle", "500ms"), want: "--idle must be at least 1s"},
		{name: "zero ready", flags: append(run, "--ready-timeout", "0"), want: "--ready-timeout must be at least 1s"},
		{name: "subsecond ready", flags: append(run, "--ready-timeout", "500ms"), want: "--ready-timeout must be at least 1s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRefusedLifetime(t, base, test.flags, test.want)
		})
	}
}

// Inheritance reads the live host. The local cache holds no recipe, so a
// failed read fails the command instead of guessing.
func assertRefusedLifetime(t *testing.T, base, flags []string, want string) {
	t.Helper()
	host := setupCommandTestHost(t)
	_, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, append(append([]string(nil), base...), flags...)...)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if events := host.recorded(); len(events) != 0 {
		t.Fatalf("a refused lifetime reached the host: %v", events)
	}
}

func TestServeInheritanceNeedsALiveRead(t *testing.T) {
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{existingRecipe(true)}
	unreachable := errors.New("host unreachable")
	dial := func(context.Context, HostRecord) (transport.Conn, error) { return nil, unreachable }
	_, _, err := executeCommand(t, Dependencies{DialControl: dial}, "serve", "pc", "5173", "--at", "/dev", "--run", "bun run dev", "--cwd", "/srv/app")
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("serve = %v, want the failed live read", err)
	}
	if run := persistedRecipe(t, host); run.Command != "./old" {
		t.Fatalf("a failed read changed the recipe to %+v", run)
	}
}

// A kept route waiting for mesh serve start is held, never idle. A held
// route reads stopping or failed until its stop has finished.
func TestHeldRoutesReadHeldEverywhere(t *testing.T) {
	held := existingRecipe(true)
	held.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped, SessionID: "7K3D", Held: true}
	row := ServiceCatalogRow{Service: held, Live: true}
	if row.State() != heldServiceState || row.Health() != heldServiceState {
		t.Fatalf("held row reads %q / %q", row.State(), row.Health())
	}
	if got := PickerServiceState(row); got != heldServiceState {
		t.Fatalf("picker reads %q", got)
	}
	catalog := projectDashboardServices([]protocol.ServiceInfo{held}, ObservedSection{}, true)
	if catalog.Held != 1 || catalog.Idle != 0 || len(catalog.Rows) != 1 || catalog.Rows[0].State != heldServiceState {
		t.Fatalf("dashboard = %+v, want one held route and no idle one", catalog)
	}
	for _, state := range []string{protocol.DemandStopping, protocol.DemandFailed} {
		row.Service.Demand.State = state
		if row.State() != state {
			t.Fatalf("held route in %s reads %q", state, row.State())
		}
	}
	finite := existingRecipe(false)
	finite.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
	if got := (ServiceCatalogRow{Service: finite, Live: true}).State(); got != protocol.DemandStopped {
		t.Fatalf("a stopped finite route reads %q", got)
	}
	if catalog := projectDashboardServices([]protocol.ServiceInfo{finite}, ObservedSection{}, true); catalog.Idle != 1 || catalog.Rows[0].State != "idle" {
		t.Fatalf("a stopped finite route on the dashboard = %+v", catalog)
	}
	if got := (ServiceCatalogRow{Service: held}).State(); got != "-" {
		t.Fatalf("an offline row reads %q, want no invented state", got)
	}
	cloned := cloneWireServices([]protocol.ServiceInfo{held})
	if !cloned[0].Run.KeepRunning || !cloned[0].Demand.Held || cloned[0].Run == held.Run {
		t.Fatalf("clone lost the kept lifetime or hold: %+v", cloned[0])
	}
}
