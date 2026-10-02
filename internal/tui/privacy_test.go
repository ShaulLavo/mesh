package tui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

const privateHost = "secret-owner-laptop"
const privatePath = "/work/private/customer-orchid"
const privateTitle = "Confidential orchid migration"
const privateScreen = "arbitrary-secret-without-a-regex-pattern"

func assertPrivateFrame(t *testing.T, frame string) {
	t.Helper()
	for _, secret := range []string{privateHost, privatePath, "customer-orchid", privateTitle, privateScreen, "secret-service", "private@example.test", "private-", "private-account-", "secret-argument", "secret-error"} {
		if strings.Contains(frame, secret) {
			t.Errorf("private value %q leaked in frame:\n%s", secret, ansi.Strip(frame))
		}
	}
}

func privacyPickerFixture() model {
	current := newPickerModel(context.Background(), cli.PickerInput{
		Privacy: privacy.New(),
		Hosts:   []cli.HostSessions{{Host: cli.HostRecord{ID: "real-host-id", Alias: privateHost}, Sessions: []protocol.SessionInfo{{ID: "7K3D", State: "detached", Cwd: privatePath, Command: []string{"/usr/bin/bash", "secret-argument"}, CreatedAt: pickerTestNow}}, Local: true}},
	}, pickerTestNow)
	current.width, current.height = 110, 32
	current.resizeList()
	return current
}

func TestPrivacyPickerFramesAndOriginalSelection(t *testing.T) {
	current := privacyPickerFixture()
	assertPrivateFrame(t, current.View().Content)
	current.enterSessions(0)
	current.hosts[0].route = "https://" + privateHost + ".ts.net"
	current.hosts[0].served = []servedWebsite{{name: "secret-service", url: "https://private@example.test/private", health: "healthy"}}
	current.hosts[0].servedKnown = true
	current.notice = "secret-error arbitrary credential"
	current.inspection = inspectionState{kind: inspectionReady, hasValue: true, target: inspectionTarget{privateHost, "7K3D"}, value: cli.SessionInspection{
		CurrentDirectory: privatePath, ForegroundCommand: "/usr/bin/bash secret-argument", TerminalTitle: privateTitle,
		Preview: []string{privateScreen}, StyledPreview: []protocol.PreviewLine{{Runs: []protocol.PreviewRun{{Text: privateScreen}}}},
	}}
	current.refreshSessionDelegate()
	for _, full := range []bool{false, true} {
		current.fullPreview = full
		frame := current.View().Content
		assertPrivateFrame(t, frame)
		if !strings.Contains(frame, privacyPreviewPlaceholder) {
			t.Fatalf("preview not explicitly withheld:\n%s", ansi.Strip(frame))
		}
		if *usageEvidenceDirectory != "" {
			name := "privacy-picker"
			if !full {
				before := current
				before.privacy = nil
				before.refreshSessionDelegate()
				writeFrameEvidence(t, "privacy-picker-before", before.View().Content, before.width, before.height, dashboardTheme("oled"))
			}
			if full {
				name = "privacy-full-preview"
			}
			writeFrameEvidence(t, name, frame, current.width, current.height, dashboardTheme("oled"))
		}
	}
	current.fullPreview = false
	before := cloneHosts(current.hosts)
	_ = current.View()
	if !reflect.DeepEqual(current.hosts, before) {
		t.Fatal("render mutated host/session data")
	}
	if current.inspection.value.Preview[0] != privateScreen {
		t.Fatal("render mutated inspection")
	}
	current.handleKey(key(tea.KeyEnter))
	selected, ok := current.selection.(attachSelection)
	if !ok || selected.hostAlias != privateHost || selected.sessionID != "7K3D" {
		t.Fatalf("masked action target: %#v", current.selection)
	}
}

func TestPrivacySavedPreviewAndWindow(t *testing.T) {
	current := privacyPickerFixture()
	current.enterSessions(0)
	current.hosts[0].sessions[0].state = "interrupted"
	current.hosts[0].sessions[0].recovery = &recovery.Record{ShellDirectory: privatePath, Title: privateTitle, Lines: []string{privateScreen}, CheckpointAt: pickerTestNow, Restart: &recovery.Command{Argv: []string{"bash", "secret-argument"}}}
	_ = current.list.SetItems(sessionItems(current.hosts[0].sessions))
	current.refreshSessionDelegate()
	frame := current.View().Content
	assertPrivateFrame(t, frame)
	if !strings.Contains(frame, privacyPreviewPlaceholder) {
		t.Fatal("saved preview not withheld")
	}
	window := newWindowModel(context.Background(), cli.WindowInput{Privacy: privacy.New(), HostID: "real-host-id", HostAlias: privateHost, Sessions: []protocol.SessionInfo{{ID: "7K3D", State: "detached", Cwd: privatePath, Command: []string{"bash", "secret-argument"}}}}, pickerTestNow)
	window.picker.inspection = current.inspection
	window.resize()
	assertPrivateFrame(t, window.View().Content)
}

func TestPrivacyDashboardRenderedFrames(t *testing.T) {
	current := usageFixture(t, "normal")
	current.privacy = privacy.New()
	for i := range current.hosts {
		h := &current.hosts[i]
		h.Host.Alias = fmt.Sprintf("%s-%d", privateHost, i+1)
		h.Sessions.Rows = []cli.DashboardSession{{ID: "7K3D", Name: fmt.Sprintf("%s %d", privateTitle, i+1), Label: fmt.Sprintf("%s %d", privateTitle, i+1), Command: "/usr/bin/bash secret-argument", State: "detached"}}
		h.Sessions.Total = 1
		h.Services.Rows = []cli.DashboardService{{Name: fmt.Sprintf("secret-service-%d", i%2+1), State: "failed", Problem: "secret-error https://private@example.test", Failed: true}}
		h.Services.Total, h.Services.Failed = 1, 1
		h.Services.Ready, h.Services.Idle, h.Services.Unknown = 0, 0, 0
	}
	for i := range current.usage.accounts {
		current.usage.accounts[i].Label = fmt.Sprintf("private-%d@example.test", i+1)
		if i == 0 {
			current.usage.accounts[i].Label = "private-account-1"
		}
	}
	current.sessionSummaries = map[dashboardSessionTarget]sessionLiveSummary{{current.hosts[0].Host.ID, "7K3D"}: {currentDirectory: privatePath, terminalTitle: privateTitle, foregroundCommand: "bash secret-argument", receivedAt: current.now}}
	before := append([]cli.DashboardHostView(nil), current.hosts...)
	display := current.privacyDisplay()
	if display.hosts[0].CPU != current.hosts[0].CPU || display.usage.accounts[0].Plan != current.usage.accounts[0].Plan || display.usage.accounts[0].Provider != current.usage.accounts[0].Provider {
		t.Fatal("privacy changed public metrics/provider/plan")
	}
	for _, size := range [][2]int{{160, 45}, {80, 24}, {110, 32}} {
		current.width, current.height = size[0], size[1]
		current.wall = true
		frame := current.View().Content
		assertPrivateFrame(t, frame)
		assertFits(t, frame, current.width, current.height)
		if !strings.Contains(frame, "account-") {
			t.Fatalf("usage account not masked:\n%s", ansi.Strip(frame))
		}
		if *usageEvidenceDirectory != "" && current.width == 160 {
			writeUsageEvidence(t, "privacy-dashboard", current)
			before := current
			before.privacy = nil
			writeUsageEvidence(t, "privacy-dashboard-before", before)
		}
	}
	if !reflect.DeepEqual(current.hosts, before) || current.usage.accounts[0].Label != "private-account-1" {
		t.Fatal("dashboard render mutated source data")
	}
	if current.visibleSessionTargets()[dashboardSessionTarget{current.hosts[0].Host.ID, "7K3D"}].HostAlias != privateHost+"-1" {
		t.Fatal("dashboard inspection target was masked")
	}
	plain := current
	plain.privacy = nil
	if !strings.Contains(plain.render(), "private-account-1") {
		t.Fatal("nil privacy should preserve account labels")
	}
}

func TestPrivacyInputOverridesEnvironment(t *testing.T) {
	t.Setenv("MESH_PRIVACY", "true")
	input := cli.PickerInput{Hosts: []cli.HostSessions{{Host: cli.HostRecord{Alias: privateHost}}}}
	current := newPickerModel(context.Background(), input, pickerTestNow)
	if !strings.Contains(current.View().Content, privateHost) {
		t.Fatal("nil input privacy was overridden by environment")
	}
}

func TestPrivacyUpdateReminderFailure(t *testing.T) {
	current := privacyPickerFixture()
	current.updateNotice.value = cli.UpdateNotice{Version: "v0.1.52", Pending: "1 unfinished update operation"}
	current.updateNotice.problem = "Could not save reminder: " + privatePath
	frame := strings.Join(current.updateNoticeLines(), "\n")
	assertPrivateFrame(t, frame)
	if !strings.Contains(frame, "v0.1.52") || !strings.Contains(frame, "unfinished update") || !strings.Contains(frame, "error-") {
		t.Fatalf("masked reminder lost public facts: %s", frame)
	}
}

func TestPrivacyForegroundEscapesExecutableControls(t *testing.T) {
	const executable = "/usr/bin/unsafe\x1b[2J\u202e"
	for _, ended := range []bool{false, true} {
		current := privacyPickerFixture()
		current.enterSessions(0)
		row := current.hosts[0].sessions[0]
		current.inspection = inspectionState{kind: inspectionReady, value: cli.SessionInspection{ForegroundCommand: executable + " secret-argument", Preview: []string{privateScreen}}}
		if ended {
			row.state = "interrupted"
			row.recovery = &recovery.Record{ShellDirectory: privatePath, Restart: &recovery.Command{Argv: []string{executable, "secret-argument"}}, Lines: []string{privateScreen}}
		}
		details := current.detailsFor(row)
		if strings.ContainsRune(details.foreground, '\x1b') || strings.ContainsRune(details.foreground, '\u202e') {
			t.Fatalf("ended=%v: unsafe foreground %q", ended, details.foreground)
		}
		if !strings.Contains(details.foreground, `\x1b[2J\u202e`) || !strings.Contains(details.foreground, "[arguments withheld]") {
			t.Fatalf("ended=%v: controls not visibly escaped: %q", ended, details.foreground)
		}
		if len(details.preview) != 1 || details.preview[0] != privacyPreviewPlaceholder {
			t.Fatalf("ended=%v: preview not withheld: %#v", ended, details.preview)
		}
	}
}

func TestPrivacyDashboardRestartNotice(t *testing.T) {
	current := dashboardModel{privacy: privacy.New(), notice: "Dashboard restart failed: " + privatePath}
	display := current.privacyDisplay()
	if strings.Contains(display.notice, privatePath) || !strings.HasPrefix(display.notice, "notice-") {
		t.Fatalf("restart notice was not masked: %q", display.notice)
	}
	if !strings.Contains(current.notice, privatePath) {
		t.Fatal("privacy display mutated the source restart notice")
	}
}
