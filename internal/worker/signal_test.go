package worker

import (
	"slices"
	"testing"
)

func TestSignalNamesMatchesSupportedSignals(t *testing.T) {
	names := SignalNames()
	if len(names) != len(signals) || !slices.IsSorted(names) {
		t.Fatalf("signal names = %v, want every supported signal in sorted order", names)
	}
	for name := range signals {
		if !slices.Contains(names, name) || !SupportsSignal(name) {
			t.Errorf("supported signal %q is missing from %v", name, names)
		}
	}
	names[0] = "not-a-signal"
	if slices.Contains(SignalNames(), "not-a-signal") {
		t.Fatal("caller changed the worker's supported signal names")
	}
}
