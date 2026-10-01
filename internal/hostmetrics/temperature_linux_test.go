//go:build linux

package hostmetrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shirou/gopsutil/v4/common"
)

func writeSensor(t *testing.T, root, path, value string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxTemperatureNamesCPUInsteadOfFirstZone(t *testing.T) {
	root := t.TempDir()
	writeSensor(t, root, "class/thermal/thermal_zone0/type", "acpitz\n")
	writeSensor(t, root, "class/thermal/thermal_zone0/temp", "90000\n")
	writeSensor(t, root, "class/thermal/thermal_zone1/type", "cpu-thermal\n")
	writeSensor(t, root, "class/thermal/thermal_zone1/temp", "42500\n")
	v, err := readTemperature(context.Background(), root)
	if err != nil || v.Sensor != "cpu-thermal" || v.Celsius != 42.5 {
		t.Fatalf("CPU sensor = %+v, %v", v, err)
	}
}

func TestLinuxTemperaturePackageAndUnsupported(t *testing.T) {
	root := t.TempDir()
	if _, err := readTemperature(context.Background(), root); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing sensor = %v", err)
	}
	writeSensor(t, root, "class/hwmon/hwmon0/name", "coretemp\n")
	writeSensor(t, root, "class/hwmon/hwmon0/temp1_label", "Core 0\n")
	writeSensor(t, root, "class/hwmon/hwmon0/temp1_input", "99999\n")
	writeSensor(t, root, "class/hwmon/hwmon0/temp2_label", "Package id 0\n")
	writeSensor(t, root, "class/hwmon/hwmon0/temp2_input", "56000\n")
	v, err := readTemperature(context.Background(), root)
	if err != nil || v.Sensor != "coretemp: Package id 0" || v.Celsius != 56 {
		t.Fatalf("package sensor = %+v,%v", v, err)
	}
}

func TestLinuxCollectorDoesNotDoubleCountGuestAndUsesMemAvailable(t *testing.T) {
	root := t.TempDir()
	writeSensor(t, root, "stat", "cpu  200 100 100 500 100 0 0 0 150 80\n")
	writeSensor(t, root, "meminfo", "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 600 kB\nCached: 400 kB\nBuffers: 50 kB\n")
	ctx := context.WithValue(context.Background(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: root})
	c := systemCollector{}
	v, err := c.CPU(ctx)
	if err != nil {
		t.Fatal(err)
	}
	percent, valid := utilization(counters{}, v)
	if !valid || percent != 40 {
		t.Fatalf("guest counted twice: %+v, percentage %v", v, percent)
	}
	memory, err := c.Memory(ctx)
	if err != nil || memory.TotalBytes != 1000*1024 || memory.AvailableBytes != 600*1024 {
		t.Fatalf("RAM available semantics = %+v, %v", memory, err)
	}
}
