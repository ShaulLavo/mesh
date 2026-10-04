package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/shaul/mesh/internal/privacy"
)

func TestFleetServicesAppearOnMain(t *testing.T) {
	current := newModel([]host{
		{id: "alpha", machineName: "alpha", served: []servedWebsite{{name: "Fregat dev", url: "https://alpha.example.test/dev", health: "healthy"}}, servedKnown: true},
		{id: "beta", machineName: "beta", served: []servedWebsite{{name: "CLI Proxy", url: "https://beta.example.test/ai", health: "healthy"}}, servedKnown: true},
	}, pickerTestNow)
	frame := ansi.Strip(current.View().Content)
	for _, want := range []string{"Fregat dev", "CLI Proxy", "Services"} {
		if !strings.Contains(frame, want) {
			t.Errorf("main picker omits %q:\n%s", want, frame)
		}
	}
}

func fleetPickerFixture() model {
	current := newPickerModel(context.Background(), cli.PickerInput{Hosts: []cli.HostSessions{
		{Host: cli.HostRecord{ID: "alpha", MachineName: "alpha"}, Local: true}, {Host: cli.HostRecord{ID: "beta", MachineName: "beta"}},
	}}, pickerTestNow)
	for _, host := range current.hosts {
		service := protocol.ServiceInfo{Name: "dev", DisplayName: "Fregat dev", Kind: "proxy", Target: "3000", Healthy: true, Run: &protocol.ServiceRun{Command: "fixture"}, Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}
		current, _ = current.applyServices(cli.PickerServicesUpdate{Host: cli.HostRecord{ID: host.id, MachineName: host.machineName}, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{{Host: cli.HostRecord{ID: host.id, MachineName: host.machineName, Endpoint: "wss://" + host.machineName + ".example.test/control/ws"}, Service: service, Live: true}}}})
	}
	current.list.Select(3)
	current.resizeList()
	return current
}

func TestFleetServiceWatchKeepsSelectionAcrossInsertionAndHostLoad(t *testing.T) {
	current := fleetPickerFixture()
	selected := mainItemKey(current.list.SelectedItem())
	updated := cli.PickerServicesUpdate{Host: cli.HostRecord{ID: "alpha", MachineName: "alpha"}, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{
		{Host: cli.HostRecord{ID: "alpha", MachineName: "alpha"}, Service: protocol.ServiceInfo{Name: "aaa", DisplayName: "New route", Healthy: true}, Live: true},
		{Host: cli.HostRecord{ID: "alpha", MachineName: "alpha"}, Service: protocol.ServiceInfo{Name: "dev", Healthy: true}, Live: true},
	}}}
	current, _ = current.applyServices(updated)
	if got := mainItemKey(current.list.SelectedItem()); got != selected {
		t.Fatalf("watch moved selection %+v -> %+v", selected, got)
	}
	current, _ = current.applyLoadedHosts(hostCatalogLoadedMsg{hosts: []cli.HostSessions{{Host: cli.HostRecord{ID: "beta", MachineName: "beta"}}}})
	if got := mainItemKey(current.list.SelectedItem()); got != selected || len(current.list.Items()) != 5 {
		t.Fatalf("host load lost service selection %+v, rows %d", got, len(current.list.Items()))
	}
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: cli.HostRecord{ID: "beta", MachineName: "beta"}})
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
	if frame := ansi.Strip(current.View().Content); !strings.Contains(frame, "idle") || strings.Contains(frame, "● stopped") || !strings.Contains(frame, "next connection starts it") {
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

func TestCopy068ServiceFeedbackObservable(t *testing.T) {
	for _, width := range []int{44, 80, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			current := fleetPickerFixture()
			current.width, current.height = width, 20
			current.resizeList()
			row := current.hosts[1].served[0].row
			current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
				return cli.PickerServiceActionResult{Row: row, Latency: 24 * time.Millisecond}, nil
			}
			next, command := current.Update(runeKey('p'))
			current = next.(model)
			next, _ = current.Update(command())
			current = next.(model)
			writeServiceCopyEvidence(t, current, fmt.Sprintf("control-%d", width))
			frame := current.View().Content
			assertFits(t, frame, width, 20)
			if !strings.Contains(ansi.Strip(frame), "ping healthy 24 ms") || mainItemKey(current.list.SelectedItem()) != (serviceTarget{"beta", "dev"}) {
				t.Fatal("known-good selected service feedback is not observable")
			}
		})
	}
}

func TestCopy068ServiceActionFailures(t *testing.T) {
	for _, example := range []struct {
		name   string
		key    tea.KeyPressMsg
		action cli.PickerServiceAction
		label  string
	}{
		{"stop", runeKey('s'), cli.PickerStopService, "Could not stop"},
		{"restart", runeKey('r'), cli.PickerRestartService, "Could not restart"},
		{"check", runeKey('p'), cli.PickerPingService, "Could not check"},
		{"open", runeKey('o'), cli.PickerOpenService, "Could not open"},
		{"enter", key(tea.KeyEnter), cli.PickerOpenService, "Could not open"},
	} {
		for _, width := range []int{44, 80, 100} {
			t.Run(fmt.Sprintf("%s-%d", example.name, width), func(t *testing.T) {
				current := fleetPickerFixture()
				current.width, current.height = width, 20
				current.resizeList()
				target := serviceTarget{"beta", "dev"}
				before := current.hosts[1].served[0].row
				cause := errors.New("fixture refused")
				calls := 0
				current.serviceAct = func(_ context.Context, request cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
					calls++
					if request != (cli.PickerServiceActionRequest{HostID: target.hostID, ServiceName: target.route, Action: example.action}) {
						t.Fatalf("selected action %+v", request)
					}
					return cli.PickerServiceActionResult{}, cause
				}
				next, command := current.Update(example.key)
				current = next.(model)
				if command == nil || current.pendingService == nil || current.selection != nil {
					t.Fatal("service action did not start in the picker")
				}
				next, duplicate := current.Update(example.key)
				current = next.(model)
				if duplicate != nil || calls != 0 {
					t.Fatal("busy service action started twice")
				}
				result := command().(serviceActionResultMsg)
				if !errors.Is(result.err, cause) || result.err.Error() != cause.Error() {
					t.Fatal("callback lost original cause")
				}
				next, _ = current.Update(result)
				current = next.(model)
				writeServiceCopyEvidence(t, current, fmt.Sprintf("%s-%d", example.name, width))
				if calls != 1 || current.pendingService != nil || current.selection != nil || mainItemKey(current.list.SelectedItem()) != target {
					t.Fatal("failure changed callback settlement or selected target")
				}
				if !reflect.DeepEqual(before, current.hosts[1].served[0].row) {
					t.Fatal("failure changed the watched service row")
				}
				want := example.label + ": " + cause.Error()
				frame := current.View().Content
				assertFits(t, frame, width, 20)
				if current.serviceFeedback[target] != want || !strings.Contains(ansi.Strip(frame), "    "+want) {
					t.Fatalf("action-specific failure absent: want %q, stored %q", want, current.serviceFeedback[target])
				}
			})
		}
	}
}

func TestCopy068ServiceFailureCauseAndPrivacy(t *testing.T) {
	for _, masked := range []bool{false, true} {
		for _, long := range []bool{false, true} {
			t.Run(fmt.Sprintf("privacy-%t-long-%t", masked, long), func(t *testing.T) {
				current := fleetPickerFixture()
				current.width, current.height = 100, 20
				cause := "fixture refused https://example.test/?hidden=fixture"
				if long {
					current.width = 44
					cause = strings.Repeat("fixture detail ", 12) + "fixture end"
				}
				if masked {
					current.privacy = privacy.New()
				}
				current.resizeList()
				current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
					return cli.PickerServiceActionResult{}, errors.New(cause)
				}
				next, command := current.Update(runeKey('r'))
				current = next.(model)
				next, _ = current.Update(command())
				current = next.(model)
				writeServiceCopyEvidence(t, current, fmt.Sprintf("privacy-%t-long-%t", masked, long))
				frame := current.View().Content
				assertFits(t, frame, current.width, 20)
				feedback := current.serviceFeedback[serviceTarget{"beta", "dev"}]
				if feedback != "Could not restart: "+cause {
					t.Fatal("stored failure discarded or changed the original cause")
				}
				visible := ansi.Strip(frame)
				if long && strings.Contains(visible, "fixture end") {
					t.Fatal("long cause unexpectedly bypassed narrow row clipping")
				}
				if !long && strings.Contains(visible, "hidden=fixture") == masked {
					t.Fatal("rendered failure changed privacy query masking")
				}
			})
		}
	}
}

func TestCopy068ServiceFailureSafeText(t *testing.T) {
	for _, masked := range []bool{false, true} {
		t.Run(fmt.Sprint(masked), func(t *testing.T) {
			current := fleetPickerFixture()
			current.width, current.height = 100, 20
			cause := "fixture refused\x1b[2J /home/fixture/project"
			path := "/home/fixture/project"
			if masked {
				current.privacy = privacy.New()
				path = "~/project"
			}
			current.resizeList()
			request := cli.PickerServiceActionRequest{HostID: "beta", ServiceName: "dev", Action: cli.PickerOpenService}
			current.pendingService = &request
			current = current.applyServiceAction(serviceActionResultMsg{request: request, err: errors.New(cause)})
			writeServiceCopyEvidence(t, current, fmt.Sprintf("safe-text-%t", masked))
			feedback := current.serviceFeedback[serviceTarget{"beta", "dev"}]
			frame := current.View().Content
			assertFits(t, frame, 100, 20)
			if !strings.HasSuffix(feedback, cause) || strings.Contains(frame, "\x1b[2J") || !strings.Contains(ansi.Strip(frame), "fixture refused\\x1b[2J "+path) {
				t.Fatal("failure changed source cause, terminal sanitation or home-path masking")
			}
		})
	}
}

func TestCopy068ServiceFailureKeepsMatchingWatchError(t *testing.T) {
	current := fleetPickerFixture()
	request := cli.PickerServiceActionRequest{HostID: "beta", ServiceName: "dev", Action: cli.PickerRestartService}
	current.pendingService = &request
	before := current.hosts[1].served[0].row
	changed := before
	changed.Service.Healthy = false
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: changed.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{changed}}})
	if current.serviceObservation == 0 {
		t.Fatal("watch fixture did not revise the pending target")
	}
	current = current.applyServiceAction(serviceActionResultMsg{request: request, err: errors.New("fixture refused")})
	if !strings.HasSuffix(current.serviceFeedback[serviceTarget{"beta", "dev"}], ": fixture refused") || !reflect.DeepEqual(current.hosts[1].served[0].row, changed) || current.pendingService != nil {
		t.Fatal("matching error changed existing watch-revision settlement")
	}
}

func TestCopy068ServiceFailureIgnoresObsoleteRequests(t *testing.T) {
	for _, condition := range []string{"host", "route", "action", "superseded", "removed", "readded"} {
		t.Run(condition, func(t *testing.T) {
			current := fleetPickerFixture()
			target := serviceTarget{"beta", "dev"}
			before := current.hosts[1].served[0].row
			request := cli.PickerServiceActionRequest{HostID: target.hostID, ServiceName: target.route, Action: cli.PickerPingService}
			current.pendingService = &request
			result := serviceActionResultMsg{request: request, err: errors.New("obsolete fixture refusal")}
			switch condition {
			case "host":
				result.request.HostID = "alpha"
			case "route":
				result.request.ServiceName = "preview"
			case "action":
				result.request.Action = cli.PickerOpenService
			case "superseded":
				newer := request
				newer.Action = cli.PickerRestartService
				current.pendingService = &newer
			case "removed", "readded":
				current, _ = current.applyServices(cli.PickerServicesUpdate{Host: before.Host})
				if condition == "readded" {
					current, _ = current.applyServices(cli.PickerServicesUpdate{Host: before.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{before}}})
				}
			}
			pending := current.pendingService
			current = current.applyServiceAction(result)
			if current.serviceFeedback[target] != "" || strings.Contains(ansi.Strip(current.View().Content), "obsolete fixture refusal") {
				t.Fatal("obsolete failure reached selected service feedback")
			}
			if condition != "removed" && condition != "readded" && current.pendingService != pending {
				t.Fatal("obsolete reply cleared the current pending request")
			}
		})
	}
}

func writeServiceCopyEvidence(t *testing.T, current model, name string) {
	t.Helper()
	if *usageEvidenceDirectory == "" {
		return
	}
	if err := os.MkdirAll(*usageEvidenceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	frame := current.View().Content
	writeFrameEvidence(t, name, frame, current.width, current.height, dashboardTheme("current"))
	if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+".ansi.txt"), []byte(frame), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := vt.NewEmulator(current.width, current.height)
	defer func() {
		if err := terminal.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := terminal.WriteString(strings.ReplaceAll(frame, "\n", "\r\n")); err != nil {
		t.Fatal(err)
	}
	rows := make([][]any, current.height)
	for y := range current.height {
		for x := range current.width {
			rows[y] = append(rows[y], terminal.CellAt(x, y))
		}
	}
	cells, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*usageEvidenceDirectory, name+".cells.json"), cells, 0o600); err != nil {
		t.Fatal(err)
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
	next, _ = current.Update(pickerServicesMsg{Host: cli.HostRecord{ID: "beta", MachineName: "beta"}, Catalog: cli.PickerServiceCatalog{Stale: true, Rows: []cli.ServiceCatalogRow{current.hosts[1].served[0].row}}, Problem: "fixture offline"})
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
		publish(cli.PickerServicesUpdate{Host: cli.HostRecord{ID: "fixture", MachineName: "fixture"}})
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

func TestCompactFleetServiceDetailsFollowSelection(t *testing.T) {
	current := fleetPickerFixture()
	current.width, current.height = 44, 20
	current.serviceFeedback[serviceTarget{"alpha", "dev"}] = "Stopped"
	current.serviceFeedback[serviceTarget{"beta", "dev"}] = "ping healthy 24 ms"
	current.resizeList()
	frame := ansi.Strip(current.View().Content)
	assertFits(t, current.View().Content, 44, 20)
	if !strings.Contains(frame, "Services") || strings.Contains(frame, "alpha.example.test/dev") || strings.Contains(frame, "Stopped") || !strings.Contains(frame, "ping healthy 24 ms") {
		t.Fatalf("details must belong only to selected service:\n%s", frame)
	}
	lines := strings.Split(frame, "\n")
	alpha, beta := -1, -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "Services" && (index == 0 || strings.TrimSpace(lines[index-1]) != "") {
			t.Fatalf("Services heading needs a blank line above:\n%s", frame)
		}
	}
	for index, line := range lines {
		if strings.Contains(line, "● Fregat dev") && strings.Contains(line, "alpha") {
			alpha = index
		}
		if strings.Contains(line, "● Fregat dev") && strings.Contains(line, "beta") {
			beta = index
		}
	}
	if alpha < 0 || beta != alpha+1 {
		t.Fatalf("service rows are not consecutive compact lines:\n%s", frame)
	}
	current = updateModel(t, current, key(tea.KeyUp))
	frame = ansi.Strip(current.View().Content)
	if !strings.Contains(frame, "alpha.example.test/dev") || strings.Contains(frame, "beta.example.test/dev") || strings.Contains(frame, "ping healthy 24 ms") {
		t.Fatalf("details did not follow selection:\n%s", frame)
	}
	assertFits(t, current.View().Content, 44, 20)
}

func TestFleetRemovedReaddedRouteRejectsOldAction(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		current := fleetPickerFixture()
		before := current.hosts[1].served[0].row
		current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
			stopped := before
			stopped.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
			return cli.PickerServiceActionResult{Row: stopped}, nil
		}
		next, command := current.Update(runeKey('s'))
		current = next.(model)
		current, _ = current.applyServices(cli.PickerServicesUpdate{Host: before.Host})
		after := before
		if renamed {
			after.Service.DisplayName = "Replacement"
			after.Service.Target = "4000"
		}
		current, _ = current.applyServices(cli.PickerServicesUpdate{Host: after.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{after}}})
		if strings.Contains(ansi.Strip(current.View().Content), "Stopping") {
			t.Fatal("readded route inherited obsolete pending label")
		}
		next, _ = current.Update(command())
		current = next.(model)
		website := current.serviceWebsite(serviceTarget{"beta", "dev"})
		if website.state != dashboardRunning || current.serviceFeedback[serviceTarget{"beta", "dev"}] != "" {
			t.Fatalf("old stop applied to readded route, renamed=%v state=%s feedback=%q", renamed, website.state, current.serviceFeedback[serviceTarget{"beta", "dev"}])
		}
	}
}

func TestFleetSuccessFeedbackDismissesOnSelectionChange(t *testing.T) {
	current := fleetPickerFixture()
	current.serviceFeedback[serviceTarget{"beta", "dev"}] = "Stopped · next connection starts it"
	current = updateModel(t, current, key(tea.KeyUp))
	current = updateModel(t, current, key(tea.KeyDown))
	if current.serviceFeedback[serviceTarget{"beta", "dev"}] != "" {
		t.Fatal("selection retained dismissed action feedback")
	}
}

func TestCompactFleetServicePaginationFits(t *testing.T) {
	current := fleetPickerFixture()
	row := current.hosts[0].served[0].row
	rows := make([]cli.ServiceCatalogRow, 24)
	for index := range rows {
		rows[index] = row
		rows[index].Service.Name = fmt.Sprintf("route%02d", index)
		rows[index].Service.DisplayName = fmt.Sprintf("Service%02d", index)
	}
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: row.Host, Catalog: cli.PickerServiceCatalog{Rows: rows}})
	for _, height := range []int{12, 20} {
		current.width, current.height = 44, height
		current.list.Select(0)
		current.resizeList()
		for range len(current.list.Items()) {
			current = updateModel(t, current, key(tea.KeyDown))
			frame := ansi.Strip(current.View().Content)
			assertFits(t, current.View().Content, 44, height)
			selected, ok := current.list.SelectedItem().(serviceItem)
			if ok && (!strings.Contains(frame, "Services") || !strings.Contains(frame, selected.website.url)) {
				t.Fatalf("selected service fell outside page:\n%s", frame)
			}
		}
	}
}

func TestCompactFleetPendingAndErrorStayInSelectedDetail(t *testing.T) {
	current := fleetPickerFixture()
	current.width, current.height = 44, 20
	current.resizeList()
	request := cli.PickerServiceActionRequest{HostID: "beta", ServiceName: "dev", Action: cli.PickerPingService}
	current.pendingService = &request
	current.refreshMainDelegate()
	frame := ansi.Strip(current.View().Content)
	assertFits(t, current.View().Content, 44, 20)
	if !strings.Contains(frame, "    Pinging…") || strings.Contains(frame, "alpha.example.test/dev") {
		t.Fatalf("pending details misplaced:\n%s", frame)
	}
	current = current.applyServiceAction(serviceActionResultMsg{request: request, err: errors.New("fixture refused")})
	frame = ansi.Strip(current.View().Content)
	if !strings.Contains(frame, "    Could not check: fixture refused") || strings.Contains(strings.Split(frame, "\n")[1], "Could not check") {
		t.Fatalf("error details misplaced:\n%s", frame)
	}
	current = updateModel(t, current, key(tea.KeyUp))
	if strings.Contains(ansi.Strip(current.View().Content), "fixture refused") {
		t.Fatal("error escaped selected-only details")
	}
	assertFits(t, current.View().Content, 44, 20)
}
