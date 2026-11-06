package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/worker"
)

func TestPeriodicHistoryCapsDoNotCountSelectedRetirements(t *testing.T) {
	for _, labelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("unrelated-labelled-%t", labelled), func(t *testing.T) {
			checkMixedHistoryRetention(t, labelled)
		})
	}
}

func checkMixedHistoryRetention(t *testing.T, labelled bool) {
	t.Helper()
	h := newHistoryCatalog(t, nil)
	for index := range 20 {
		writeMixedHistoryRecord(t, h.root, fmt.Sprintf("B%03d", index), "service hot", historyBase.Add(-time.Duration(index+1)*time.Minute), true)
	}
	for index := range 490 {
		addMixedHistoryRecord(t, h, index, labelled)
	}
	observed, err := h.catalog.scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	kept, retired := h.catalog.retireFinishedSessions(observed)
	if len(kept) != maxRetainedTerminalSessions || len(retired) != 10 {
		t.Fatalf("mixed retention kept %d and retired %d; want 500 and 10", len(kept), len(retired))
	}
	for _, id := range retired {
		if strings.HasPrefix(string(id), "C") {
			t.Fatalf("hot-label churn unnecessarily retired unrelated history %s", id)
		}
	}
}

func addMixedHistoryRecord(t *testing.T, h historyCatalog, index int, labelled bool) {
	t.Helper()
	id := fmt.Sprintf("C%03d", index)
	created := historyBase.Add(-time.Duration(index+100) * time.Minute)
	if labelled {
		writeMixedHistoryRecord(t, h.root, id, fmt.Sprintf("service cold-%d", index), created, true)
		return
	}
	writeMixedHistoryRecord(t, h.root, id, "", created, false)
}

func writeMixedHistoryRecord(t *testing.T, root, id, label string, created time.Time, running bool) {
	t.Helper()
	meta := worker.Meta{ID: id, PID: 4242, Command: []string{"sh", "-lc", "true"}, Label: label, State: worker.StateRunning, BootID: "previous-boot", CreatedAt: created}
	if !running {
		exited, code := created.Add(time.Second), 0
		meta.State, meta.ExitedAt, meta.ExitCode = worker.StateExited, &exited, &code
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Meta(dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
