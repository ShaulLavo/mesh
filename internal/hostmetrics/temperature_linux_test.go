//go:build linux

package hostmetrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNamedCPUSensorRejectsGPUAndInvalidReadings(t *testing.T) {
	root := t.TempDir()
	write := func(zone, field, value string) {
		t.Helper()
		dir := filepath.Join(root, "class/thermal", zone)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, field), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("thermal_zone0", "type", "gpu")
	write("thermal_zone0", "temp", "61000")
	if _, _, err := readComponentSensors(context.Background(), root); !errors.Is(err, ErrUnsupported) {
		t.Fatal("GPU mistaken for CPU", err)
	}
	write("thermal_zone1", "type", "cpu-thermal")
	write("thermal_zone1", "temp", "NaN")
	if _, _, err := readComponentSensors(context.Background(), root); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatal("invalid CPU reading hidden", err)
	}
	write("thermal_zone1", "temp", "42500")
	_, value, err := readComponentSensors(context.Background(), root)
	if err != nil || value.Sensor != "cpu-thermal" || value.Celsius != 42.5 {
		t.Fatal(value, err)
	}
	write("thermal_zone1", "temp", string(make([]byte, 257)))
	if _, _, err := readComponentSensors(context.Background(), root); err == nil {
		t.Fatal("unbounded sensor read")
	}
}
