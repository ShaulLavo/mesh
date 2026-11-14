//go:build mesh_installed_fixture

package update

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type installedOrderReadTrace struct {
	Complete bool             `json:"complete"`
	Spans    []map[string]any `json:"spans"`
	output   string
}

func newInstalledOrderReadTrace() *installedOrderReadTrace {
	return &installedOrderReadTrace{Complete: true, output: os.Getenv("MESH_INSTALLED_ORDER_TRACE_OUTPUT")}
}

func (r *installedOrderReadTrace) stamp() map[string]int64 {
	var monotonic unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		r.Complete = false
	}
	return map[string]int64{"monotonicNs": monotonic.Nano(), "wallNs": time.Now().UnixNano()}
}

func (r *installedOrderReadTrace) begin(phase string) func(error) {
	if r.output == "" {
		return func(error) {}
	}
	start := r.stamp()
	return func(err error) {
		value := map[string]any{"phase": phase, "start": start, "end": r.stamp(), "success": err == nil}
		if err != nil {
			value["error"] = boundedProblem(err.Error())
		}
		r.Spans = append(r.Spans, value)
	}
}

func (r *installedOrderReadTrace) write(t *testing.T, root *os.Root) {
	t.Helper()
	if r.output == "" {
		return
	}
	data, err := json.Marshal(r)
	if err == nil && r.output == "order-reader-timeline.json" && len(data) <= 64<<10 {
		err = root.WriteFile(r.output, data, 0600)
	} else {
		t.Logf("order reader diagnostic incomplete: invalid output or encoding: %v", err)
		return
	}
	if err != nil {
		t.Logf("order reader diagnostic write failed: %v", err)
	}
}

func TestInstalledOrderTraceOnly(t *testing.T) {
	t.Setenv("MESH_INSTALLED_ORDER_TRACE_OUTPUT", "order-reader-timeline.json")
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	trace := newInstalledOrderReadTrace()
	trace.begin("mock.failed.read")(errors.New("synthetic read failure"))
	trace.write(t, root)
	data, err := root.ReadFile("order-reader-timeline.json")
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Complete bool `json:"complete"`
		Spans    []struct {
			Success bool             `json:"success"`
			Error   string           `json:"error"`
			Start   map[string]int64 `json:"start"`
			End     map[string]int64 `json:"end"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if !value.Complete || len(value.Spans) != 1 || value.Spans[0].Success || value.Spans[0].Error != "synthetic read failure" {
		t.Fatalf("reader trace = %s", data)
	}
	if value.Spans[0].Start["monotonicNs"] > value.Spans[0].End["monotonicNs"] || value.Spans[0].Start["wallNs"] <= 0 {
		t.Fatalf("reader timestamps = %s", data)
	}
}
