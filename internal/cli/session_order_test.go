package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/worker"
)

func TestLocalListOrdersBySavedUpdate(t *testing.T) {
	setupCommandTestHost(t)
	for index, id := range []string{"7K3D", "91AZ"} {
		dir, err := paths.SessionDir(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		meta := worker.Meta{ID: id, State: worker.StateDetached, Command: []string{"bash"}, Cwd: "/work", CreatedAt: commandTestTime.Add(time.Duration(index) * time.Hour)}
		if err := worker.WriteMeta(dir, meta); err != nil {
			t.Fatal(err)
		}
	}
	dir, _ := paths.SessionDir("7K3D")
	saved := recovery.Record{Version: recovery.Version, HostID: "test-host", SessionID: "7K3D",
		Shell: "/bin/bash", ShellDirectory: "/work", DirectorySource: recovery.DirectoryLaunch,
		Command: []string{"bash"}, CheckpointAt: commandTestTime.Add(2 * time.Hour)}
	if err := recovery.Write(dir, saved); err != nil {
		t.Fatal(err)
	}
	rows, err := List()
	if err != nil || len(rows) != 2 {
		t.Fatalf("local rows = %v, %v", rows, err)
	}
	if rows[0].ID != "7K3D" {
		t.Fatalf("first local session = %s, want recently updated 7K3D", rows[0].ID)
	}
}

func TestPrintedSessionListOrdersBySavedUpdate(t *testing.T) {
	rows := []protocol.SessionInfo{
		{ID: "OLD", CreatedAt: commandTestTime, Recovery: &recovery.Record{CheckpointAt: commandTestTime.Add(2 * time.Hour)}},
		{ID: "NEW", CreatedAt: commandTestTime.Add(time.Hour)},
	}
	var output bytes.Buffer
	if err := writeProtocolSessions(&output, commandTestTime, []HostSessions{{Host: HostRecord{Alias: "pc"}, Sessions: rows}}); err != nil {
		t.Fatal(err)
	}
	if strings.Index(output.String(), "OLD") > strings.Index(output.String(), "NEW") {
		t.Fatalf("printed sessions sorted by creation instead of update:\n%s", &output)
	}
}
