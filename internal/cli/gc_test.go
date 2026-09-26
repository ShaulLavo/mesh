package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/worker"
)

func gcRow(id, state string, detachedAgo, quietAgo time.Duration, agent bool) protocol.SessionInfo {
	now := commandTestTime
	row := protocol.SessionInfo{
		ID: id, HostID: "host-id", Command: []string{"bash"}, Cwd: "/work", State: state,
		CreatedAt: now.Add(-72 * time.Hour), MemoryBytes: 100 << 20,
		Recovery: &recovery.Record{LastOutputAt: now.Add(-quietAgo)},
	}
	if detachedAgo > 0 {
		detached := now.Add(-detachedAgo)
		row.DetachedAt = &detached
	}
	if agent {
		row.Command = []string{"claude"}
		row.Recovery.Agent = &agentresume.Recipe{Launch: agentresume.Launch{Provider: agentresume.Claude}, ConversationID: "c", Lifecycle: agentresume.Active}
	}
	return row
}

func TestPlanGCSelectsIdleDetachedSessions(t *testing.T) {
	const idle = 6 * time.Hour
	closedAgent := gcRow("C1C1", worker.StateDetached, 8*time.Hour, 8*time.Hour, true)
	closedAgent.Recovery.Agent.Lifecycle = agentresume.Closed
	attachedLongAgo := commandTestTime.Add(-9 * time.Hour)
	legacy := gcRow("L0G0", worker.StateDetached, 0, 9*time.Hour, true)
	legacy.LastAttachedAt = &attachedLongAgo
	attachedRecently := commandTestTime.Add(-time.Hour)
	legacyRecent := gcRow("L1G1", worker.StateDetached, 0, 9*time.Hour, true)
	legacyRecent.LastAttachedAt = &attachedRecently
	served := gcRow("V1V1", worker.StateDetached, 7*time.Hour, 7*time.Hour, false)
	served.Label = "serve /dev"

	for _, test := range []struct {
		name       string
		row        protocol.SessionInfo
		shells     bool
		containing bool
		want       gcAction
		note       string
	}{
		{name: "attached", row: gcRow("A1A1", worker.StateRunning, 0, 9*time.Hour, true)},
		{name: "detached recently", row: gcRow("A2A2", worker.StateDetached, time.Hour, 9*time.Hour, true)},
		{name: "detached long ago but printed recently", row: gcRow("A3A3", worker.StateDetached, 9*time.Hour, time.Minute, true)},
		{name: "exited", row: gcRow("A4A4", worker.StateExited, 9*time.Hour, 9*time.Hour, true)},
		{name: "idle agent", row: gcRow("A5A5", worker.StateDetached, 7*time.Hour, 8*time.Hour, true), want: gcHibernate},
		{name: "idle agent exactly at the limit", row: gcRow("A6A6", worker.StateDetached, idle, idle, true), want: gcHibernate},
		{name: "closed conversation is a shell", row: closedAgent, want: gcLeave, note: "plain shell"},
		{name: "idle shell without --shells", row: gcRow("S1S1", worker.StateDetached, 7*time.Hour, 7*time.Hour, false), want: gcLeave, note: "plain shell"},
		{name: "idle shell with --shells", row: gcRow("S2S2", worker.StateDetached, 7*time.Hour, 7*time.Hour, false), shells: true, want: gcKill},
		{name: "missing detach time falls back to last attach", row: legacy, want: gcHibernate, note: "detach time unknown; idle counted from last attach"},
		{name: "missing detach time with a recent attach", row: legacyRecent},
		{name: "served session with --shells", row: served, shells: true, want: gcLeave, note: "serve /dev"},
		{name: "caller's own session", row: gcRow("M1M1", worker.StateDetached, 7*time.Hour, 7*time.Hour, true), shells: true, containing: true, want: gcLeave, note: "contains this terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := gcPolicy{now: commandTestTime, idle: idle, shells: test.shells}
			if test.containing {
				policy.containing = []protocol.SessionIdentity{{HostID: "other-host", SessionID: "M1M1"}, {HostID: "host-id", SessionID: test.row.ID}}
			}
			entries := planGC(policy, []HostSessions{{Host: HostRecord{Alias: "pc", ID: "host-id"}, Sessions: []protocol.SessionInfo{test.row}}})
			if test.want == "" {
				if len(entries) != 0 {
					t.Fatalf("planned %#v, want the session left alone", entries)
				}
				return
			}
			if len(entries) != 1 || entries[0].action != test.want {
				t.Fatalf("plan = %#v, want %s", entries, test.want)
			}
			if test.note != "" && !strings.Contains(gcActionText(entries[0]), test.note) {
				t.Fatalf("action %q does not say %q", gcActionText(entries[0]), test.note)
			}
		})
	}
}

func TestPlanGCSkipsStaleHostsAndDuplicateEntries(t *testing.T) {
	row := gcRow("A5A5", worker.StateDetached, 7*time.Hour, 8*time.Hour, true)
	policy := gcPolicy{now: commandTestTime, idle: 6 * time.Hour}
	entries := planGC(policy, []HostSessions{
		{Local: true, Host: HostRecord{Alias: localHostAlias}, Sessions: []protocol.SessionInfo{row}},
		{Host: HostRecord{Alias: "self", ID: "host-id"}, Sessions: []protocol.SessionInfo{row}},
		{Host: HostRecord{Alias: "offline", ID: "other"}, Stale: true, Sessions: []protocol.SessionInfo{{ID: "B1B1", HostID: "other", State: worker.StateDetached, CreatedAt: commandTestTime.Add(-72 * time.Hour)}}},
	})
	if len(entries) != 1 || !entries[0].local {
		t.Fatalf("plan = %#v, want one local entry", entries)
	}
}

func TestGCPrintsAPlanAndActsOnlyWithYes(t *testing.T) {
	host := setupCommandTestHost(t)
	host.listRows = func() []protocol.SessionInfo {
		agent := gcRow("7K3D", worker.StateDetached, 7*time.Hour, 8*time.Hour, true)
		agent.HostID = host.host.ID
		agent.Recovery.Title = "Refactor parser"
		shell := gcRow("91AZ", worker.StateDetached, 7*time.Hour, 8*time.Hour, false)
		shell.HostID = host.host.ID
		shell.MemoryBytes = 0
		return []protocol.SessionInfo{agent, shell}
	}
	dependencies := Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}
	stdout, _, err := executeCommand(t, dependencies, "gc")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOST", "MEM", "ACTION", "hibernate", "claude · Refactor parser", "left running (plain shell)", "reclaimable: 100M from 1 session", "mesh gc --yes"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("gc plan omitted %q:\n%s", want, stdout)
		}
	}
	if events := host.recorded(); host.eventCount(protocol.TypeHibernate) != 0 || host.eventCount(protocol.TypeKill) != 0 {
		t.Fatalf("gc without --yes acted: %v", events)
	}

	stdout, _, err = executeCommand(t, dependencies, "gc", "--yes", "--idle", "7h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "hibernated 7K3D on pc") || !strings.Contains(stdout, "reclaimed 100M from 1 session") {
		t.Fatalf("gc outcome:\n%s", stdout)
	}
	request := host.actedOn()
	if request.Type != protocol.TypeHibernate || request.SessionID != "7K3D" || request.HibernateIdleMillis != (7*time.Hour).Milliseconds() {
		t.Fatalf("gc request = %#v, want an idle-checked hibernation of 7K3D", request)
	}
	if host.eventCount(protocol.TypeKill) != 0 {
		t.Fatal("gc killed a plain shell without --shells")
	}
}

func TestGCRejectsNonPositiveIdle(t *testing.T) {
	setupCommandTestHost(t)
	if _, _, err := executeCommand(t, Dependencies{}, "gc", "--idle", "0s"); err == nil || !strings.Contains(err.Error(), "--idle") {
		t.Fatalf("gc --idle 0s = %v", err)
	}
}

func TestGCRechecksAShellBeforeKillingIt(t *testing.T) {
	host := setupCommandTestHost(t)
	host.listRows = func() []protocol.SessionInfo {
		shell := gcRow("91AZ", worker.StateDetached, 7*time.Hour, 8*time.Hour, false)
		shell.HostID = host.host.ID
		return []protocol.SessionInfo{shell}
	}
	// The fake host's inspection reports the session attached, as if someone
	// reattached between the listing and the kill.
	stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "gc", "--shells", "--yes")
	if err == nil || !strings.Contains(err.Error(), "session 91AZ on pc: attached since the plan was made") {
		t.Fatalf("gc kill of a reattached shell = %v\n%s", err, stdout)
	}
	if host.eventCount(protocol.TypeInspect) != 1 || host.eventCount(protocol.TypeKill) != 0 {
		t.Fatalf("events = %v, want one recheck and no kill", host.recorded())
	}
}

func TestReclaimCommandsReserveAliases(t *testing.T) {
	for _, name := range []string{"gc", "hibernate"} {
		if _, err := ValidateHostAlias(name); err == nil {
			t.Fatalf("host alias %q would be shadowed by its command", name)
		}
	}
}
