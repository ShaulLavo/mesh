//go:build linux

package hostmetrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixtureField(t *testing.T, root, path, value string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestComponentInventoryHottestAndPiDie(t *testing.T) {
	root := t.TempDir()
	for _, v := range [][3]string{{"hwmon0", "coretemp", "69000"}, {"hwmon1", "nvme", "39000"}, {"hwmon2", "nvme", "41000"}, {"hwmon3", "spd5118", "38000"}, {"hwmon4", "spd5118", "36000"}} {
		fixtureField(t, root, "class/hwmon/"+v[0]+"/name", v[1])
		fixtureField(t, root, "class/hwmon/"+v[0]+"/temp1_input", v[2])
		fixtureField(t, root, "class/hwmon/"+v[0]+"/temp1_label", "Package id 0")
	}
	values, _, err := readComponentSensors(context.Background(), root)
	if err != nil || len(values) != 3 || values[0].Label != "CPU" || values[0].Celsius != 69 || values[1].Label != "NVMe" || values[1].Celsius != 41 || values[2].Label != "RAM" {
		t.Fatal(values, err)
	}
	pi := t.TempDir()
	fixtureField(t, pi, "class/thermal/thermal_zone0/type", "cpu-thermal")
	fixtureField(t, pi, "class/thermal/thermal_zone0/temp", "48000")
	values, _, err = readComponentSensors(context.Background(), pi)
	if err != nil || len(values) != 1 || values[0].Kind != "soc" || values[0].Label != "SoC" {
		t.Fatal(values, err)
	}
}
func TestNVIDIASingleBoundedQueryAndMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	if _, _, err := readNVIDIA(t.Context()); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	fixtureField(t, root, "nvidia-smi", "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$NVIDIA_ARGS\"\nprintf '37, 3072, 8192, 52\\n'\n")
	// #nosec G302 -- executable fixture requires user execute permission.
	if err := os.Chmod(filepath.Join(root, "nvidia-smi"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NVIDIA_ARGS", filepath.Join(root, "args"))
	gpu, temp, err := readNVIDIA(t.Context())
	if err != nil || gpu.Utilization != 37 || gpu.MemoryUsedBytes != 3<<30 || temp != 52 {
		t.Fatal(gpu, temp, err)
	}
	args, _ := os.ReadFile(filepath.Join(root, "args")) // #nosec G304 -- test-owned temporary query receipt.
	if string(args) != "--query-gpu=utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits\n" {
		t.Fatal(string(args))
	}
	fixtureField(t, root, "nvidia-smi", "#!/bin/sh\nwhile :; do :; done\n")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := readNVIDIA(ctx); err == nil || time.Since(start) > time.Second {
		t.Fatal("unbounded GPU query", err)
	}
}
func TestLinuxPhysicalDiskAndRealNetworkFilters(t *testing.T) {
	root := t.TempDir()
	fixtureField(t, root, "sys/class/block/nvme0n1/device/id", "physical")
	fixtureField(t, root, "sys/class/block/nvme0n1p1/partition", "1")
	fixtureField(t, root, "sys/class/block/sda/device/id", "physical")
	fixtureField(t, root, "proc/diskstats", "259 0 nvme0n1 1 0 20 0 1 0 40 0 0 50 0\n259 1 nvme0n1p1 1 0 999 0 1 0 999 0 0 999 0\n8 0 sda 1 0 10 0 1 0 30 0 0 20 0\n7 0 loop0 1 0 999 0 1 0 999 0 0 999 0\n")
	disk, err := readDiskCounters(t.Context(), filepath.Join(root, "proc"), filepath.Join(root, "sys"))
	if err != nil || disk.ReadBytes != 30*512 || disk.WriteBytes != 70*512 || disk.BusyMillis != 70 {
		t.Fatal(disk, err)
	}
	fixtureField(t, root, "proc/net/dev", "Inter-| Receive | Transmit\n eth0: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n lo: 900 0 0 0 0 0 0 0 900 0 0 0 0 0 0 0\n tailscale0: 800 0 0 0 0 0 0 0 800 0 0 0 0 0 0 0\n vethx: 700 0 0 0 0 0 0 0 700 0 0 0 0 0 0 0\n")
	net, err := readNetworkCounters(t.Context(), filepath.Join(root, "proc"))
	if err != nil || net.ReceiveBytes != 100 || net.SendBytes != 200 {
		t.Fatal(net, err)
	}
}
func TestLinuxBatteryChargeState(t *testing.T) {
	root := t.TempDir()
	fixtureField(t, root, "class/power_supply/BAT0/type", "Battery")
	fixtureField(t, root, "class/power_supply/BAT0/present", "1")
	fixtureField(t, root, "class/power_supply/BAT0/capacity", "73")
	fixtureField(t, root, "class/power_supply/BAT0/status", "Discharging")
	fixtureField(t, root, "class/power_supply/BAT0/energy_now", "42000000")
	fixtureField(t, root, "class/power_supply/BAT0/power_now", "14000000")
	value, err := readBattery(t.Context(), root)
	if err != nil || value.Percent != 73 || value.State != "discharging" || value.SecondsRemaining != 10800 {
		t.Fatal(value, err)
	}
}

type inventoryPerformanceFixture struct {
	performanceFixture
	native *systemCollector
}

func (c *inventoryPerformanceFixture) Temperature(ctx context.Context) (Temperature, error) {
	return c.native.Temperature(ctx)
}
func (c *inventoryPerformanceFixture) Components(ctx context.Context) ([]ComponentTemperature, error) {
	return c.native.Components(ctx)
}
func (c *inventoryPerformanceFixture) GPU(ctx context.Context) (GPU, error) { return c.native.GPU(ctx) }

func TestNVIDIAKeepsInventoryFailureAndIndependentLastGood(t *testing.T) {
	root := t.TempDir()
	fixtureField(t, root, "bin/nvidia-smi", "#!/bin/sh\nprintf '37, 3072, 8192, 52\\n'\n")
	// #nosec G302 -- executable fixture requires user execute permission.
	if err := os.Chmod(filepath.Join(root, "bin/nvidia-smi"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(root, "bin"))
	fixtureField(t, root, "class/hwmon/hwmon0/name", "coretemp")
	fixtureField(t, root, "class/hwmon/hwmon0/temp1_label", "Package id 0")
	fixtureField(t, root, "class/hwmon/hwmon0/temp1_input", "69000")
	now := time.Now()
	collector := &inventoryPerformanceFixture{native: &systemCollector{temperatureRoot: root}}
	sampler := NewWithCollector(collector, func() time.Time { return now })
	first, err := sampler.Read(t.Context())
	if err != nil || first.TemperaturesFailing || len(first.Temperatures) != 2 || first.Temperatures[0].Celsius != 69 || first.GPU == nil || first.GPU.Failing {
		t.Fatal("known-good sysfs and NVIDIA", first, err)
	}
	fixtureField(t, root, "class/hwmon/hwmon0/temp1_input", "broken")
	now = now.Add(10 * time.Second)
	failed, err := sampler.Read(t.Context())
	if err != nil || !failed.TemperaturesFailing || len(failed.Temperatures) != 2 || failed.Temperatures[0].Celsius != 69 || failed.Temperatures[0].AgeMillis < 10000 || failed.GPU == nil || failed.GPU.Failing || failed.GPU.AgeMillis != 0 || failed.GPU.Value.Utilization != 37 {
		t.Fatal("GPU success masked real inventory failure", failed, err)
	}
	if failed.Temperatures[1].Kind != KindGPU || failed.Temperatures[1].AgeMillis > 100 {
		t.Fatal("independent GPU temperature stopped updating", failed.Temperatures)
	}
	collector.native.temperatureRoot = t.TempDir()
	fresh := NewWithCollector(collector, func() time.Time { return now })
	gpuOnly, err := fresh.Read(t.Context())
	if err != nil || gpuOnly.TemperaturesFailing || len(gpuOnly.Temperatures) != 1 || gpuOnly.Temperatures[0].Kind != KindGPU {
		t.Fatal("GPU-only host invents an inventory failure", gpuOnly, err)
	}
}
