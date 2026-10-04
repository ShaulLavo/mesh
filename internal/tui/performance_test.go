package tui

import (
	"context"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func benchmarkPicker(b *testing.B) (model, cli.PickerHostSnapshot) {
	b.Helper()
	rows := make([]protocol.SessionInfo, 20)
	for index := range rows {
		rows[index] = protocol.SessionInfo{ID: fmt.Sprintf("%04d", index), HostID: "bench-host",
			State: "detached", Command: []string{"bash"}, Cwd: "/work/projects/mesh", CreatedAt: time.Unix(1700000000, 0)}
	}
	host := cli.HostSessions{Host: cli.HostRecord{ID: "bench-host", MachineName: "bench"}, Sessions: rows}
	current := newPickerModel(context.Background(), cli.PickerInput{Hosts: []cli.HostSessions{host}}, time.Unix(1700000060, 0))
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	current = updated.(model)
	current.showSessions()
	if current.screen != sessionScreen || current.inspection.target.sessionID == "" {
		b.Fatal("benchmark must exercise the selected session")
	}
	return current, cli.PickerHostSnapshot{Sessions: host}
}

func BenchmarkPickerUnchangedCatalog20(b *testing.B) {
	current, snapshot := benchmarkPicker(b)
	message := catalogRefreshResultMsg{epoch: current.catalogEpoch, hostID: "bench", snapshot: snapshot}
	b.ReportAllocs()
	for b.Loop() {
		current, _ = current.applyCatalogRefresh(message)
	}
}

func BenchmarkInspectorUnchangedAndRender20(b *testing.B) {
	current, _ := benchmarkPicker(b)
	preview := make([]string, 12)
	for index := range preview {
		preview[index] = "INFO build: compiled package; tests passed; duration=12ms"
	}
	message := inspectionResultMsg{target: current.inspection.target, generation: current.inspection.generation,
		value: cli.SessionInspection{ObservedAt: current.now, CurrentDirectory: "/work/projects/mesh", Preview: preview}}
	b.ReportAllocs()
	for b.Loop() {
		current = current.applyInspection(message)
		_ = current.View()
	}
}
