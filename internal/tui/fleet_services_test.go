package tui

import (
	"context"
	"errors"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestFleetServicesAppearOnMain(t *testing.T) {
	current := newModel([]host{
		{id: "alpha", alias: "alpha", served: []servedWebsite{{name: "Fregat dev", url: "https://alpha.example.test/dev", health: "healthy"}}, servedKnown: true},
		{id: "beta", alias: "beta", served: []servedWebsite{{name: "CLI Proxy", url: "https://beta.example.test/ai", health: "healthy"}}, servedKnown: true},
	}, pickerTestNow)
	frame := ansi.Strip(current.View().Content)
	for _, want := range []string{"Fregat dev", "CLI Proxy", "alpha.example.test/dev", "beta.example.test/ai"} {
		if !strings.Contains(frame, want) {
			t.Errorf("main picker omits %q:\n%s", want, frame)
		}
	}
}

func fleetPickerFixture() model {
	current := newPickerModel(context.Background(), cli.PickerInput{Hosts: []cli.HostSessions{
		{Host: cli.HostRecord{ID: "alpha", Alias: "alpha"}, Local: true}, {Host: cli.HostRecord{ID: "beta", Alias: "beta"}},
	}}, pickerTestNow)
	for _, host := range current.hosts {
		service := protocol.ServiceInfo{Name: "dev", DisplayName: "Fregat dev", Kind: "proxy", Target: "3000", Healthy: true, Run: &protocol.ServiceRun{Command: "fixture"}, Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}
		current, _ = current.applyServices(cli.PickerServicesUpdate{Host: cli.HostRecord{ID: host.id, Alias: host.alias}, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{{Host: cli.HostRecord{ID: host.id, Alias: host.alias, Endpoint: "wss://" + host.alias + ".example.test/control/ws"}, Service: service, Live: true}}}})
	}
	current.list.Select(3)
	current.resizeList()
	return current
}

func TestFleetServiceWatchKeepsSelectionAcrossInsertionAndHostLoad(t *testing.T) {
	current := fleetPickerFixture()
	selected := mainItemKey(current.list.SelectedItem())
	updated := cli.PickerServicesUpdate{Host: cli.HostRecord{ID: "alpha", Alias: "alpha"}, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{
		{Host: cli.HostRecord{ID: "alpha", Alias: "alpha"}, Service: protocol.ServiceInfo{Name: "aaa", DisplayName: "New route", Healthy: true}, Live: true},
		{Host: cli.HostRecord{ID: "alpha", Alias: "alpha"}, Service: protocol.ServiceInfo{Name: "dev", Healthy: true}, Live: true},
	}}}
	current, _ = current.applyServices(updated)
	if got := mainItemKey(current.list.SelectedItem()); got != selected {
		t.Fatalf("watch moved selection %+v -> %+v", selected, got)
	}
	current, _ = current.applyLoadedHosts(hostCatalogLoadedMsg{hosts: []cli.HostSessions{{Host: cli.HostRecord{ID: "beta", Alias: "beta"}}}})
	if got := mainItemKey(current.list.SelectedItem()); got != selected || len(current.list.Items()) != 5 {
		t.Fatalf("host load lost service selection %+v, rows %d", got, len(current.list.Items()))
	}
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: cli.HostRecord{ID: "beta", Alias: "beta"}})
	if len(current.list.Items()) != 4 || current.list.Index() >= 4 {
		t.Fatalf("removed selection out of bounds %d", current.list.Index())
	}
}

func TestFleetServiceKeysTargetSelectedHostRouteAndStayOpen(t *testing.T) {
	for _, example := range []struct {
		key    rune
		action cli.PickerServiceAction
	}{{'s', cli.PickerStopService}, {'r', cli.PickerRestartService}, {'p', cli.PickerPingService}, {'o', cli.PickerOpenService}} {
		t.Run(string(example.key), func(t *testing.T) {
			current := fleetPickerFixture()
			calls := 0
			current.serviceAct = func(_ context.Context, request cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
				calls++
				if request.HostID != "beta" || request.ServiceName != "dev" || request.Action != example.action {
					t.Fatalf("selected action %+v", request)
				}
				row := current.hosts[1].served[0].row
				return cli.PickerServiceActionResult{Row: row, Latency: 24 * time.Millisecond}, nil
			}
			next, command := current.Update(runeKey(example.key))
			current = next.(model)
			if command == nil || current.selection != nil || current.pendingService == nil {
				t.Fatal("service action closed picker or did not start")
			}
			next, duplicate := current.Update(runeKey(example.key))
			current = next.(model)
			if duplicate != nil {
				t.Fatal("busy service action started twice")
			}
			message := command()
			next, _ = current.Update(message)
			current = next.(model)
			if calls != 1 || current.pendingService != nil || current.selection != nil {
				t.Fatalf("action settlement calls=%d pending=%v selection=%v", calls, current.pendingService, current.selection)
			}
			frame := ansi.Strip(current.View().Content)
			if example.action == cli.PickerPingService && !strings.Contains(frame, "ping healthy 24 ms") {
				t.Fatalf("ping result absent:\n%s", frame)
			}
		})
	}
}

func TestFleetServicesStopResultKeepsObservedIdle(t *testing.T) {
	current := fleetPickerFixture()
	row := current.hosts[1].served[0].row
	row.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
	update := cli.PickerServicesUpdate{Host: row.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{row}}}
	current, _ = current.applyServices(update)
	if current.hosts[1].served[0].state != "idle" {
		t.Fatal("auto-stopped demand should be idle")
	}
	request := cli.PickerServiceActionRequest{HostID: "beta", ServiceName: "dev", Action: cli.PickerStopService}
	current.pendingService = &request
	current = current.applyServiceAction(serviceActionResultMsg{request: request, result: cli.PickerServiceActionResult{Row: row}})
	if frame := ansi.Strip(current.View().Content); !strings.Contains(frame, "● idle") || strings.Contains(frame, "● stopped") || !strings.Contains(frame, "next connection starts it") {
		t.Fatalf("stop result changed observed idle:\n%s", frame)
	}
	current, _ = current.applyServices(update)
	if current.hosts[1].served[0].state != "idle" {
		t.Fatal("stop result changed observed state")
	}
	row.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandRunning}
	update.Catalog.Rows = []cli.ServiceCatalogRow{row}
	current, _ = current.applyServices(update)
	if current.serviceFeedback[serviceTarget{"beta", "dev"}] != "" {
		t.Fatal("next connection retained previous stop result")
	}
}

func TestFleetServiceFailureVisibleWithoutTerminatingPicker(t *testing.T) {
	current := fleetPickerFixture()
	current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
		return cli.PickerServiceActionResult{}, errors.New("fixture health check refused")
	}
	next, command := current.Update(runeKey('p'))
	current = next.(model)
	next, _ = current.Update(command())
	current = next.(model)
	frame := ansi.Strip(current.View().Content)
	if !strings.Contains(frame, "fixture health check refused") || current.selection != nil || current.pendingService != nil {
		t.Fatalf("failure closed picker:\n%s", frame)
	}
	next, _ = current.Update(pickerServicesMsg{Host: cli.HostRecord{ID: "beta", Alias: "beta"}, Catalog: cli.PickerServiceCatalog{Stale: true, Rows: []cli.ServiceCatalogRow{current.hosts[1].served[0].row}}, Problem: "fixture offline"})
	current = next.(model)
	if frame = ansi.Strip(current.View().Content); !strings.Contains(frame, "fixture offline") || !strings.Contains(frame, "cached") || current.selection != nil {
		t.Fatalf("watch failure hidden:\n%s", frame)
	}
}

func TestFleetServiceFooterFitsNarrowAndNoUnserve(t *testing.T) {
	current := fleetPickerFixture()
	current.width, current.height = 44, 20
	current.resizeList()
	frame := ansi.Strip(current.View().Content)
	assertFits(t, current.View().Content, 44, 20)
	for _, want := range []string{"s stop", "r restart", "p ping", "o open", "esc cancel"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing footer %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "unserve") || strings.Contains(frame, "delete") {
		t.Fatal("permanent deletion exposed")
	}
}

func TestFleetServiceEvidence(t *testing.T) {
	if *usageEvidenceDirectory == "" {
		t.Skip("pass -args -usage-evidence-dir DIR to export picker fixtures")
	}
	if err := os.MkdirAll(*usageEvidenceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	current := fleetPickerFixture()
	idle := current.hosts[0].served[0].row
	idle.Service.DisplayName = "Fregat dev"
	idle.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: idle.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{idle}}})
	current.hosts[1].served[0].name = "CLI Proxy"
	broken := current.hosts[1].served[0].row
	broken.Service.Name = "gallery"
	broken.Service.DisplayName = "Comfy Gallery"
	broken.Service.Healthy = false
	broken.Service.Problem = "fixture check refused"
	current.hosts[1].served = append(current.hosts[1].served, servedWebsites([]cli.ServiceCatalogRow{broken}, false)...)
	stopped := idle
	stopped.Service.Name = "preview"
	stopped.Service.DisplayName = "Preview"
	current.hosts[0].served = append(current.hosts[0].served, servedWebsites([]cli.ServiceCatalogRow{stopped}, false)...)
	current.serviceFeedback[serviceTarget{"alpha", "preview"}] = "Stopped · next connection starts it"
	current.serviceFeedback[serviceTarget{"beta", "dev"}] = "ping healthy 24 ms"
	_ = current.resetMainItems(serviceTarget{"beta", "dev"}, 0)
	for _, size := range []struct {
		name          string
		width, height int
	}{{"picker-services-normal", 100, 24}, {"picker-services-narrow", 44, 20}} {
		current.width, current.height = size.width, size.height
		current.resizeList()
		writeFrameEvidence(t, size.name, current.View().Content, current.width, current.height, dashboardTheme("current"))
	}
}

func TestFleetServiceRemovedDuringActionDoesNotKeepFeedback(t *testing.T) {
	current := fleetPickerFixture()
	row := current.hosts[1].served[0].row
	request := cli.PickerServiceActionRequest{HostID: "beta", ServiceName: "dev", Action: cli.PickerPingService}
	current.pendingService = &request
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: row.Host})
	current = current.applyServiceAction(serviceActionResultMsg{request: request, result: cli.PickerServiceActionResult{Row: row}})
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: row.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{row}}})
	if current.serviceFeedback[serviceTarget{"beta", "dev"}] != "" {
		t.Fatal("re-added route inherited removed route's action result")
	}
}

func TestFleetServiceProgramCancelsAndJoinsWatcher(t *testing.T) {
	input, send := io.Pipe()
	defer func() { _ = input.Close() }()
	defer func() { _ = send.Close() }()
	started, exited := make(chan struct{}), make(chan struct{})
	current := newModel(pickerFixture(), pickerTestNow)
	current.watchServices = func(ctx context.Context, publish func(cli.PickerServicesUpdate)) error {
		close(started)
		publish(cli.PickerServicesUpdate{Host: cli.HostRecord{ID: "fixture", Alias: "fixture"}})
		<-ctx.Done()
		close(exited)
		return nil
	}
	output := newTriggerWriter(enterAltScreen, func() {
		<-started
		_, _ = io.WriteString(send, "\r\r")
		_ = send.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	final, err := runProgram(ctx, current, input, output)
	if err != nil {
		t.Fatal(err)
	}
	if final.(model).selection == nil {
		t.Fatal("picker failed to select fixture session")
	}
	select {
	case <-exited:
	default:
		t.Fatal("picker returned before joining service watcher")
	}
	_, _ = io.WriteString(output, attachStarted)
	assertRestoredBeforeAttach(t, output.String())
}

func TestFleetServiceActionKeepsNewerWatchedRow(t *testing.T) {
	current := fleetPickerFixture()
	old := current.hosts[1].served[0].row
	current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
		return cli.PickerServiceActionResult{Row: old, Latency: time.Millisecond}, nil
	}
	next, command := current.Update(runeKey('p'))
	current = next.(model)
	newer := old
	newer.Service.Healthy = false
	newer.Service.Problem = "fixture listener closed during query"
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: newer.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{newer}}})
	next, _ = current.Update(command())
	current = next.(model)
	if current.hosts[1].served[0].state != "unhealthy" {
		t.Fatal("older action reply overwrote newer watched health")
	}
}
