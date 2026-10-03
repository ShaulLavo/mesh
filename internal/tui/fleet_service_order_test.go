package tui

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestFleetServicePingHealthABARejectsOlderReply(t *testing.T) {
	current := fleetPickerFixture()
	healthy := current.hosts[1].served[0].row
	healthy.Service.Run, healthy.Service.Demand = nil, nil
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: healthy.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{healthy}}})
	unhealthy := healthy
	unhealthy.Service.Healthy = false
	current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
		return cli.PickerServiceActionResult{Row: unhealthy, Latency: 24 * time.Millisecond}, nil
	}
	next, command := current.Update(runeKey('p'))
	current = next.(model)
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: healthy.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{unhealthy}}})
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: healthy.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{healthy}}})
	next, _ = current.Update(command())
	current = next.(model)
	if current.hosts[1].served[0].state != dashboardRunning || current.serviceFeedback[serviceTarget{"beta", "dev"}] != "" {
		t.Fatalf("old unhealthy ping replaced recovered observation: state=%s feedback=%q", current.hosts[1].served[0].state, current.serviceFeedback[serviceTarget{"beta", "dev"}])
	}
}

func TestFleetServiceStopReplyAfterAutomaticRestartKeepsWatch(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		current := fleetPickerFixture()
		running := current.hosts[1].served[0].row
		running.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandRunning, SessionID: "OLD1"}
		current, _ = current.applyServices(cli.PickerServicesUpdate{Host: running.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{running}}})
		stopped := running
		stopped.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
		current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
			return cli.PickerServiceActionResult{Row: stopped}, nil
		}
		next, command := current.Update(runeKey('s'))
		current = next.(model)
		current, _ = current.applyServices(cli.PickerServicesUpdate{Host: stopped.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{stopped}}})
		if restarted {
			running.Service.Demand = &protocol.ServiceDemand{State: protocol.DemandRunning, SessionID: "NEW1"}
			current, _ = current.applyServices(cli.PickerServicesUpdate{Host: running.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{running}}})
		}
		next, _ = current.Update(command())
		current = next.(model)
		feedback := current.serviceFeedback[serviceTarget{"beta", "dev"}]
		if !restarted {
			if current.hosts[1].served[0].state != "idle" || feedback != "Stopped · next connection starts it" {
				t.Fatalf("matching watched stop lost result: state=%s feedback=%q", current.hosts[1].served[0].state, feedback)
			}
			continue
		}
		if current.hosts[1].served[0].row.Service.Demand.SessionID != "NEW1" || feedback != "" {
			t.Fatalf("old stop resurrected feedback after new session: feedback=%q", feedback)
		}
	}
}

func TestFleetServiceUnchangedAndOtherHostWatchKeepActionResult(t *testing.T) {
	current := fleetPickerFixture()
	row := current.hosts[1].served[0].row
	current.serviceAct = func(context.Context, cli.PickerServiceActionRequest) (cli.PickerServiceActionResult, error) {
		return cli.PickerServiceActionResult{Row: row, Latency: 24 * time.Millisecond}, nil
	}
	next, command := current.Update(runeKey('p'))
	current = next.(model)
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: row.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{row}}})
	other := current.hosts[0].served[0].row
	other.Service.Healthy = false
	current, _ = current.applyServices(cli.PickerServicesUpdate{Host: other.Host, Catalog: cli.PickerServiceCatalog{Rows: []cli.ServiceCatalogRow{other}}})
	next, _ = current.Update(command())
	current = next.(model)
	if feedback := current.serviceFeedback[serviceTarget{"beta", "dev"}]; feedback != "ping healthy 24 ms" {
		t.Fatalf("unrelated observation discarded result: %q", feedback)
	}
}
