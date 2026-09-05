package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestPickerRendersLocalBeforeLoadingRemoteHosts(t *testing.T) {
	calls := 0
	input := cli.PickerInput{
		Hosts: []cli.HostSessions{
			{Host: cli.HostRecord{ID: "pc-id", Alias: "pc"}, Stale: true},
			{Host: cli.HostRecord{ID: "laptop-id", Alias: "laptop"}, Local: true, Sessions: []protocol.SessionInfo{{ID: "7K3D", State: "detached"}}},
		},
		LoadHosts: func(context.Context) ([]cli.HostSessions, error) {
			calls++
			return []cli.HostSessions{
				{Host: cli.HostRecord{ID: "laptop-id", Alias: "laptop"}, Local: true, Sessions: []protocol.SessionInfo{{ID: "WRONG", State: "running"}}},
				{Host: cli.HostRecord{ID: "pc-id", Alias: "pc"}, Sessions: []protocol.SessionInfo{{ID: "91AZ", State: "running"}}},
			}, nil
		},
	}
	current := newPickerModel(context.Background(), input, pickerTestNow)
	view := ansi.Strip(current.View().Content)
	if calls != 0 || !strings.Contains(view, "this host") || strings.Index(view, "laptop") > strings.Index(view, "pc") {
		t.Fatalf("local first render calls %d:\n%s", calls, view)
	}
	command := current.Init()
	if command == nil || calls != 0 {
		t.Fatalf("Init called loader synchronously: %d calls", calls)
	}
	updated, _ := current.Update(command())
	current = updated.(model)
	if calls != 1 || current.hosts[0].sessions[0].id != "7K3D" || current.hosts[1].sessions[0].id != "91AZ" || current.hosts[1].stale {
		t.Fatalf("loaded hosts = %#v; calls %d", current.hosts, calls)
	}
	assertFits(t, current.View().Content, 80, 24)
}

func TestDelayedCatalogPreservesOpenHostAndCurrentSelection(t *testing.T) {
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{
			{Host: cli.HostRecord{ID: "laptop-id", Alias: "laptop"}, Local: true},
			{Host: cli.HostRecord{ID: "pc-id", Alias: "pc"}, Sessions: []protocol.SessionInfo{{ID: "91AZ", State: "detached"}}},
		},
		OpenHostAlias: "pc",
	}, pickerTestNow)
	updated, _ := current.Update(hostCatalogLoadedMsg{hosts: []cli.HostSessions{
		{Host: cli.HostRecord{ID: "pc-id", Alias: "pc"}, Sessions: []protocol.SessionInfo{{ID: "7K3D", State: "detached"}}},
		{Host: cli.HostRecord{ID: "pi-id", Alias: "pi"}},
	}})
	current = updated.(model)
	if current.screen != sessionScreen || current.currentHost().alias != "pc" || current.selectedSessionID() != "91AZ" || len(current.hosts) != 3 {
		t.Fatalf("delayed catalog moved selection: host %q, session %q, hosts %d", current.currentHost().alias, current.selectedSessionID(), len(current.hosts))
	}
}

func TestPickerRelaunchAndExplicitTakeoverSelection(t *testing.T) {
	for _, test := range []struct {
		state    string
		relaunch bool
		takeOver bool
		footer   string
	}{
		{state: "interrupted", relaunch: true, footer: "enter relaunch"},
		{state: "running", takeOver: true, footer: "enter take over"},
		{state: "detached", footer: "enter attach"},
	} {
		t.Run(test.state, func(t *testing.T) {
			current := newPickerModel(context.Background(), cli.PickerInput{
				Hosts:         []cli.HostSessions{{Host: cli.HostRecord{Alias: "laptop"}, Local: true, Sessions: []protocol.SessionInfo{{ID: "7K3D", State: test.state}}}},
				OpenHostAlias: "laptop",
			}, pickerTestNow)
			view := ansi.Strip(current.View().Content)
			if !strings.Contains(view, test.footer) {
				t.Fatalf("footer lacks %q:\n%s", test.footer, view)
			}
			current = updateModel(t, current, key(tea.KeyEnter))
			selected := cliSelection(current.selection)
			if selected.Relaunch != test.relaunch || selected.TakeOver != test.takeOver || selected.SessionID != "7K3D" {
				t.Fatalf("selection = %#v", selected)
			}
			assertFits(t, view, 80, 24)
		})
	}
}

func TestNestedSessionLabelsResolveHostAndContainingSession(t *testing.T) {
	local := protocol.SessionIdentity{HostID: "laptop-id", SessionID: "7K3D"}
	remote := protocol.SessionIdentity{HostID: "pc-id", SessionID: "91AZ"}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{
			{Host: cli.HostRecord{ID: local.HostID, Alias: "laptop"}, Local: true, Sessions: []protocol.SessionInfo{{ID: local.SessionID, State: "running"}}},
			{Host: cli.HostRecord{ID: remote.HostID, Alias: "pc"}, Sessions: []protocol.SessionInfo{{ID: remote.SessionID, State: "running"}}},
		},
		ContainingSessions: []cli.PickerContainingSession{
			{Identity: remote},
			{Identity: local, Snapshot: &cli.SessionInspection{Nested: []protocol.SessionIdentity{remote}}},
		},
		OpenHostAlias: "laptop",
	}, pickerTestNow)
	current.refreshSessionDelegate()
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "on pc/91AZ") {
		t.Fatalf("local nested label missing:\n%s", view)
	}
	current.openHostSessions("pc")
	current.refreshSessionDelegate()
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "via 7K3D") || !strings.Contains(view, "enter take over") {
		t.Fatalf("remote containment label missing:\n%s", view)
	}
}

func TestNestedLabelsEscapeHostNames(t *testing.T) {
	label := nestedSessionLabel([]protocol.SessionIdentity{{HostID: "pc-id", SessionID: "91AZ"}}, map[string]string{"pc-id": "pc\x1b[2J\n"})
	if strings.ContainsAny(label, "\x1b\n") {
		t.Fatalf("unsafe nesting label = %q", label)
	}
}
