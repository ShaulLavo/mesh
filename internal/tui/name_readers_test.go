package tui

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestOwnerNameDashboardCellEvidence(t *testing.T) {
	ids := []string{base64.RawURLEncoding.EncodeToString(make([]byte, 32)), base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))}
	for _, size := range [][2]int{{80, 24}, {160, 48}} {
		t.Run(fmt.Sprint(size[0]), func(t *testing.T) {
			owners := []cli.DashboardHost{{ID: ids[0], MachineName: "local-own", NameRevision: 1, NameVerified: true, Local: true}, {ID: ids[1], MachineName: "garden", NameRevision: 1, NameVerified: true}}
			model := newDashboard(cli.DashboardInput{Hosts: owners, Wall: true}, pickerTestNow)
			resized, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			model = resized.(dashboardModel)
			for _, owner := range owners {
				message := cli.DashboardHostView{Host: owner, Connection: cli.StateReachable, LastReply: pickerTestNow, NameObservedAt: pickerTestNow, PerformanceVersion: 1}
				updated, _ := model.Update(dashboardHostMsg(message))
				model = updated.(dashboardModel)
			}
			render := func(stage string, now time.Time) string {
				t.Helper()
				ticked, _ := model.Update(dashboardTickMsg(now))
				model = ticked.(dashboardModel)
				view := model.View().Content
				assertFits(t, view, size[0], size[1])
				fmt.Printf("\nBEGIN_OWNER_NAMES_%d_%s\n%s\nEND_OWNER_NAMES_%d_%s\n", size[0], stage, view, size[0], stage)
				return ansi.Strip(view)
			}
			fresh := render("FRESH", pickerTestNow)
			if !strings.Contains(fresh, "local-own") || !strings.Contains(fresh, "garden") || strings.Contains(fresh, "last known name") {
				t.Fatal("fresh destination names are not observable")
			}
			localIndex := 0
			for index, host := range model.hosts {
				if host.Host.ID == ids[0] {
					localIndex = index
				}
			}
			changed := model.hosts[localIndex]
			changed.Host.MachineName, changed.Host.NameRevision = "garden", 2
			updated, _ := model.Update(dashboardHostMsg(changed))
			model = updated.(dashboardModel)
			conflict := render("RENAMED_CONFLICT", pickerTestNow)
			if strings.Contains(conflict, "local-own") || !strings.Contains(conflict, "conflict") || !strings.Contains(conflict, "priority") {
				t.Fatal("owner rename conflict is not visible")
			}
			for _, id := range ids {
				if !strings.Contains(conflict, id) {
					t.Fatal("conflicting name hides exact destination ID")
				}
			}
			if !model.hosts[localIndex].Host.Local || model.hosts[localIndex].Host.ID != ids[0] || !model.hosts[localIndex].Host.NamePriority {
				t.Fatal("rename changed own-card identity or deterministic priority")
			}
			stale := render("RETAINED", pickerTestNow.Add(time.Minute))
			if !strings.Contains(stale, "last known name") {
				t.Fatal("retained name freshness is not visible")
			}
			changed = model.hosts[localIndex]
			changed.Host.MachineName, changed.Host.NameRevision = "local-new", 3
			changed.NameObservedAt, changed.LastReply = model.now, model.now
			updated, _ = model.Update(dashboardHostMsg(changed))
			model = updated.(dashboardModel)
			reconnected := render("RECONNECTED", model.now)
			if !strings.Contains(reconnected, "local-new") || strings.Contains(reconnected, "conflict") {
				t.Fatal("reconnect did not remove obsolete name conflict")
			}
		})
	}
}

func TestOwnerRenamePreservesPickerSelectionAndActionIdentity(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {160, 48}} {
		input := cli.PickerInput{Hosts: []cli.HostSessions{{Host: cli.HostRecord{ID: "stable-local", MachineName: "local-own", NameRevision: 1, NameVerified: true}, Sessions: []protocol.SessionInfo{{ID: "7K3D", HostID: "stable-local", State: "running", Command: []string{"shell"}, CreatedAt: pickerTestNow}}}}}
		current := newModel(hostCatalog(input), pickerTestNow)
		current = updateModel(t, current, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		current = updateModel(t, current, key(tea.KeyEnter))
		before := current.View().Content
		if !strings.Contains(ansi.Strip(before), "local-own") {
			t.Fatal("picker known-good name not observable")
		}
		snapshot := input.Hosts[0]
		snapshot.Host.MachineName, snapshot.Host.NameRevision = "local-new", 2
		updated, _ := current.Update(catalogRefreshResultMsg{epoch: current.catalogEpoch, hostID: "stable-local", snapshot: cli.PickerHostSnapshot{Sessions: snapshot}})
		current = updated.(model)
		after := current.View().Content
		assertFits(t, after, size[0], size[1])
		if !strings.Contains(ansi.Strip(after), "local-new") || strings.Contains(ansi.Strip(after), "local-own") || current.selectedSessionID() != "7K3D" {
			t.Fatal("rename failed to preserve picker selection and replace name")
		}
		fmt.Printf("\nBEGIN_PICKER_NAMES_%d_BEFORE\n%s\nEND_PICKER_NAMES_%d_BEFORE\nBEGIN_PICKER_NAMES_%d_AFTER\n%s\nEND_PICKER_NAMES_%d_AFTER\n", size[0], before, size[0], size[0], after, size[0])
		current = updateModel(t, current, key(tea.KeyEnter))
		selected, ok := current.selection.(attachSelection)
		if !ok || selected.hostID != "stable-local" || selected.sessionID != "7K3D" {
			t.Fatalf("rename changed action destination: %#v", current.selection)
		}
	}
}
