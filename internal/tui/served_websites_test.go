package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestPickerShowsServedWebsitesInOpenHostPanel(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id", TailscaleName: "pc.example.ts.net"}
	sessions := cli.HostSessions{Host: host, Sessions: []protocol.SessionInfo{{
		ID: "7K3D", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
	}}}
	refreshCalls := 0
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{sessions},
		Refresh: func(_ context.Context, alias string) (cli.PickerHostSnapshot, error) {
			refreshCalls++
			if alias != "pc" {
				t.Fatalf("refresh alias = %q, want pc", alias)
			}
			services := []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}
			if refreshCalls > 1 {
				services = append(services, protocol.ServiceInfo{Name: "status", Kind: "proxy", Target: "4000", Healthy: false})
			}
			return servedWebsiteSnapshot(host, sessions, services...), nil
		},
	}, pickerTestNow)
	current.width = 80
	current.height = 24

	if refreshCalls != 0 {
		t.Fatalf("refresh calls before opening host = %d, want 0", refreshCalls)
	}
	command := current.showSessions()
	if command == nil {
		t.Fatal("opening a host did not start live refresh")
	}
	updated, nextRefresh := current.Update(catalogRefreshMessage(t, command))
	refreshed := updated.(model)
	if refreshCalls != 1 || nextRefresh == nil {
		t.Fatalf("refresh calls = %d, next command nil %t", refreshCalls, nextRefresh == nil)
	}
	view := ansi.Strip(refreshed.View().Content)

	for _, want := range []string{"1 served", "served websites", "healthy", "https://pc.mesh.example/blog"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker view does not contain %q:\n%s", want, view)
		}
	}
	if got := refreshed.selectedSessionID(); got != "7K3D" {
		t.Fatalf("selected session after service refresh = %q, want 7K3D", got)
	}
	assertFits(t, refreshed.View().Content, 80, 24)

	updated, command = refreshed.Update(catalogRefreshTickMsg{epoch: refreshed.catalogEpoch, at: pickerTestNow})
	refreshed = updated.(model)
	updated, nextRefresh = refreshed.Update(command())
	refreshed = updated.(model)
	if refreshCalls != 2 || nextRefresh == nil {
		t.Fatalf("live refresh calls = %d, next command nil %t", refreshCalls, nextRefresh == nil)
	}
	view = ansi.Strip(refreshed.View().Content)
	if !strings.Contains(view, "2 served") || !strings.Contains(view, "https://pc.mesh.example/status") || !strings.Contains(view, "unhealthy") {
		t.Fatalf("live service addition is missing:\n%s", view)
	}
	if got := refreshed.selectedSessionID(); got != "7K3D" {
		t.Fatalf("selected session after service addition = %q, want 7K3D", got)
	}
}

func TestPickerRetainsLastServedWebsitesWhenServiceRefreshFails(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	sessions := cli.HostSessions{Host: host}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts:   []cli.HostSessions{sessions},
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) { return cli.PickerHostSnapshot{}, nil },
	}, pickerTestNow)
	current.enterSessions(0)
	live := servedWebsiteSnapshot(host, sessions, protocol.ServiceInfo{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true})
	current, _ = current.applyCatalogRefresh(catalogRefreshResultMsg{
		epoch: current.catalogEpoch, hostAlias: "pc", snapshot: live,
	})
	current, _ = current.applyCatalogRefresh(catalogRefreshResultMsg{
		epoch: current.catalogEpoch, hostAlias: "pc", snapshot: cli.PickerHostSnapshot{Sessions: sessions},
	})

	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "1 served cached") || !strings.Contains(view, "https://pc.mesh.example/blog") || !strings.Contains(view, "offline/stale") {
		t.Fatalf("failed service refresh did not retain and stale the last view:\n%s", view)
	}

	current, _ = current.applyCatalogRefresh(catalogRefreshResultMsg{
		epoch: current.catalogEpoch, hostAlias: "pc",
		snapshot: cli.PickerHostSnapshot{Sessions: sessions, Services: &cli.PickerServiceCatalog{}},
	})
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "0 served") || strings.Contains(view, "pc.mesh.example/blog") {
		t.Fatalf("authoritative empty service list did not clear the last view:\n%s", view)
	}
}

func TestCompactPickerKeepsDetailsAlongsideServedWebsites(t *testing.T) {
	current := newModel(pickerFixture()[:1], pickerTestNow)
	current.width = 52
	current.height = 16
	current.enterSessions(0)
	current.hosts[0].servedKnown = true
	for _, name := range []string{"one", "two", "three", "four"} {
		current.hosts[0].served = append(current.hosts[0].served, servedWebsite{
			url: "https://pc.mesh.example/" + name, health: "healthy",
		})
	}
	current.resizeList()
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "served websites  ·  4 total") || !strings.Contains(view, "https://pc.mesh.example/one") || !strings.Contains(view, "┌─") {
		t.Fatalf("compact picker dropped websites or session details:\n%s", view)
	}
	assertFits(t, current.View().Content, 52, 16)
}

func TestPublicWebsiteDoesNotClaimReachabilityWithoutEdgeStatus(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	websites := servedWebsites([]cli.ServiceCatalogRow{{
		Host: host, Live: true,
		Service: protocol.ServiceInfo{
			Name: "blog", Kind: "proxy", Target: "3000", PublicName: "site.example.com", Healthy: true,
		},
	}}, false)
	if len(websites) != 1 || websites[0].health != "edge-unknown" {
		t.Fatalf("public website health = %#v, want edge-unknown", websites)
	}
}

func servedWebsiteSnapshot(host cli.HostRecord, sessions cli.HostSessions, services ...protocol.ServiceInfo) cli.PickerHostSnapshot {
	rows := make([]cli.ServiceCatalogRow, len(services))
	for index, service := range services {
		rows[index] = cli.ServiceCatalogRow{Host: host, PrivateName: "pc.mesh.example", Service: service, Live: true}
	}
	return cli.PickerHostSnapshot{Sessions: sessions, Services: &cli.PickerServiceCatalog{Rows: rows}}
}
