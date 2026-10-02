//go:build linux

package hostmetrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func readComponentSensors(ctx context.Context, root string) ([]ComponentTemperature, Temperature, error) {
	thermal, legacy, thermalErr := readThermalComponents(ctx, root)
	hwmon, packageValue, hwmonErr := readHwmonComponents(ctx, root)
	if packageValue.Sensor != "" && (legacy.Sensor == "" || packageValue.Celsius > legacy.Celsius) {
		legacy = packageValue
	}
	values := HottestTemperatures(append(thermal, hwmon...))
	if err := ctx.Err(); err != nil {
		return nil, Temperature{}, fmt.Errorf("component temperatures: %w", err)
	}
	var failures []error
	for _, err := range []error{thermalErr, hwmonErr} {
		if err != nil && !errors.Is(err, ErrUnsupported) {
			failures = append(failures, err)
		}
	}
	if len(values) > 0 || len(failures) > 0 {
		return values, legacy, errors.Join(failures...)
	}
	return nil, Temperature{}, ErrUnsupported
}
func readThermalComponents(ctx context.Context, root string) ([]ComponentTemperature, Temperature, error) {
	entries, err := sensorDirs(filepath.Join(root, "class/thermal"))
	if err != nil {
		return nil, Temperature{}, err
	}
	var values []ComponentTemperature
	var legacy Temperature
	var failures []error
	for _, entry := range entries {
		dir := filepath.Join(root, "class/thermal", entry.Name())
		name, err := sensorText(ctx, filepath.Join(dir, "type"))
		if err != nil || !cpuSensor(name) {
			continue
		}
		reading, err := sensorValue(ctx, name, filepath.Join(dir, "temp"))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		kind := KindCPU
		if name == "cpu-thermal" || name == "bcm2835_thermal" {
			kind = KindSoC
		}
		values = append(values, ComponentTemperature{Kind: kind, Celsius: reading.Celsius})
		if legacy.Sensor == "" || reading.Celsius > legacy.Celsius {
			legacy = reading
		}
	}
	return values, legacy, errors.Join(failures...)
}
func readHwmonComponents(ctx context.Context, root string) ([]ComponentTemperature, Temperature, error) {
	entries, err := sensorDirs(filepath.Join(root, "class/hwmon"))
	if err != nil {
		return nil, Temperature{}, err
	}
	var values []ComponentTemperature
	var legacy Temperature
	var failures []error
	for _, entry := range entries {
		dir := filepath.Join(root, "class/hwmon", entry.Name())
		name, err := sensorText(ctx, filepath.Join(dir, "name"))
		if err != nil {
			continue
		}
		kind := hwmonKind(name)
		if kind == "" {
			continue
		}
		sensors, err := readHwmonSensorFields(ctx, dir, name, kind)
		if err != nil {
			failures = append(failures, err)
		}
		values = append(values, sensors...)
		legacy = hottestLegacyCPU(legacy, sensors, name)
	}
	return values, legacy, errors.Join(failures...)
}
func readHwmonSensorFields(ctx context.Context, dir, name, kind string) ([]ComponentTemperature, error) {
	var values []ComponentTemperature
	var failures []error
	for i := 1; i <= maximumSensors; i++ {
		prefix := filepath.Join(dir, fmt.Sprintf("temp%d", i))
		label, _ := sensorText(ctx, prefix+"_label")
		if kind == KindCPU && !packageSensor(label) {
			continue
		}
		reading, err := sensorValue(ctx, name+": "+label, prefix+"_input")
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
			continue
		}
		values = append(values, ComponentTemperature{Kind: kind, Celsius: reading.Celsius})
	}
	return values, errors.Join(failures...)
}
func hwmonKind(name string) string {
	switch name {
	case "coretemp", "k10temp", "zenpower":
		return KindCPU
	case "amdgpu", "nouveau", "i915":
		return KindGPU
	case KindNVMe:
		return KindNVMe
	case "spd5118", "jc42":
		return KindRAM
	}
	return ""
}
func (c *systemCollector) GPU(ctx context.Context) (GPU, error) {
	value, temperature, err := readNVIDIA(ctx)
	if err == nil {
		c.sensorMu.Lock()
		c.gpuTemperature = temperature
		c.gpuTemperatureAt = time.Now()
		c.sensorMu.Unlock()
	}
	return value, err
}

type boundedOutput struct {
	data  []byte
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.limit {
		return 0, errors.New("hardware command output exceeds bound")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func readNVIDIA(ctx context.Context) (GPU, float64, error) {
	path, err := exec.LookPath("nvidia-smi")
	if errors.Is(err, exec.ErrNotFound) {
		return GPU{}, 0, ErrUnsupported
	}
	if err != nil {
		return GPU{}, 0, fmt.Errorf("NVIDIA command: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--query-gpu=utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits") // #nosec G204 -- fixed read-only arguments; executable resolved from daemon PATH.
	output := &boundedOutput{limit: 4096}
	command.Stdout = output
	command.Stderr = io.Discard
	command.WaitDelay = 100 * time.Millisecond
	if err := command.Run(); err != nil {
		return GPU{}, 0, fmt.Errorf("NVIDIA counters: %w", err)
	}
	return parseNVIDIA(string(output.data))
}
func parseNVIDIA(data string) (GPU, float64, error) {
	var result GPU
	temperature := -273.15
	lines := strings.Split(strings.TrimSpace(data), "\n")
	if len(lines) > 8 {
		return GPU{}, 0, errors.New("NVIDIA GPU count exceeds eight")
	}
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) != 4 {
			return GPU{}, 0, errors.New("invalid NVIDIA counter fields")
		}
		values := make([]float64, 4)
		for i, text := range fields {
			value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
			if err != nil || !finite(value) || value < 0 {
				return GPU{}, 0, errors.New("invalid NVIDIA counters")
			}
			values[i] = value
		}
		if values[0] > 100 || values[1] > values[2] || values[2] > 1<<30 || values[3] > 1000 {
			return GPU{}, 0, errors.New("NVIDIA counters exceed bounds")
		}
		result.Utilization = max(result.Utilization, values[0])
		result.MemoryUsedBytes += uint64(values[1] * (1 << 20))
		result.MemoryTotalBytes += uint64(values[2] * (1 << 20))
		temperature = max(temperature, values[3])
	}
	return result, temperature, nil
}
func (c *systemCollector) Disk(ctx context.Context) (DiskCounters, error) {
	return readDiskCounters(ctx, c.procPath(), c.sysPath())
}
func (c *systemCollector) Network(ctx context.Context) (NetworkCounters, error) {
	return readNetworkCounters(ctx, c.procPath())
}
func (c *systemCollector) Battery(ctx context.Context) (Battery, error) {
	return readBattery(ctx, c.sysPath())
}
func (c *systemCollector) procPath() string {
	if c.procRoot != "" {
		return c.procRoot
	}
	return "/proc"
}
func (c *systemCollector) sysPath() string {
	if c.temperatureRoot != "" {
		return c.temperatureRoot
	}
	return "/sys"
}
func boundedFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hardware counter context: %w", err)
	}
	f, err := os.Open(path) // #nosec G304 -- fixed procfs counter paths, replaced only by fixtures.
	if err != nil {
		return nil, fmt.Errorf("hardware counter open: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if len(data) > 256*1024 {
		return nil, errors.New("hardware counter file exceeds bound")
	}
	if err != nil {
		return nil, fmt.Errorf("hardware counter read: %w", err)
	}
	return data, nil
}
func readDiskCounters(ctx context.Context, proc, sys string) (DiskCounters, error) {
	data, err := boundedFile(ctx, filepath.Join(proc, "diskstats"))
	if err != nil {
		return DiskCounters{}, fmt.Errorf("disk counters: %w", err)
	}
	result := DiskCounters{BusyAvailable: true}
	var devices []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 || !physicalDisk(sys, fields[2]) {
			continue
		}
		current, err := parseDiskCounters(fields)
		if err != nil {
			return DiskCounters{}, err
		}
		if !addCounter(&result.ReadBytes, current.ReadBytes) || !addCounter(&result.WriteBytes, current.WriteBytes) || !addCounter(&result.BusyMillis, current.BusyMillis) {
			return DiskCounters{}, errors.New("disk counter sum exceeds bound")
		}
		devices = append(devices, fields[2])
	}
	if err := scanner.Err(); err != nil {
		return DiskCounters{}, fmt.Errorf("disk counter lines: %w", err)
	}
	if len(devices) == 0 {
		return DiskCounters{}, ErrUnsupported
	}
	sort.Strings(devices)
	result.Devices = strings.Join(devices, ",")
	return result, nil
}
func physicalDisk(sys, name string) bool {
	for _, prefix := range []string{"loop", "ram", "dm-", "zram"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	dir := filepath.Join(sys, "class/block", name)
	if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "device"))
	return err == nil
}
func parseDiskCounters(fields []string) (DiskCounters, error) {
	values := make([]uint64, 3)
	for i, index := range []int{5, 9, 12} {
		v, err := strconv.ParseUint(fields[index], 10, 64)
		if err != nil {
			return DiskCounters{}, fmt.Errorf("disk counter number: %w", err)
		}
		values[i] = v
	}
	if values[0] > ^uint64(0)/512 || values[1] > ^uint64(0)/512 {
		return DiskCounters{}, errors.New("disk sector counter exceeds bound")
	}
	return DiskCounters{ReadBytes: values[0] * 512, WriteBytes: values[1] * 512, BusyMillis: values[2]}, nil
}
func readNetworkCounters(ctx context.Context, proc string) (NetworkCounters, error) {
	data, err := boundedFile(ctx, filepath.Join(proc, "net/dev"))
	if err != nil {
		return NetworkCounters{}, fmt.Errorf("network counters: %w", err)
	}
	var result NetworkCounters
	var devices []string
	for _, line := range strings.Split(string(data), "\n") {
		name, text, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !found || !realInterface(name) {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < 16 {
			return NetworkCounters{}, errors.New("invalid network counter fields")
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return NetworkCounters{}, fmt.Errorf("network counter number: %w", err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return NetworkCounters{}, fmt.Errorf("network counter number: %w", err)
		}
		if !addCounter(&result.ReceiveBytes, rx) || !addCounter(&result.SendBytes, tx) {
			return NetworkCounters{}, errors.New("network counter sum exceeds bound")
		}
		devices = append(devices, name)
	}
	if len(devices) == 0 {
		return NetworkCounters{}, ErrUnsupported
	}
	sort.Strings(devices)
	result.Devices = strings.Join(devices, ",")
	return result, nil
}
func readBattery(ctx context.Context, root string) (Battery, error) {
	entries, err := sensorDirs(filepath.Join(root, "class/power_supply"))
	if err != nil {
		return Battery{}, err
	}
	for _, entry := range entries {
		dir := filepath.Join(root, "class/power_supply", entry.Name())
		kind, _ := sensorText(ctx, filepath.Join(dir, "type"))
		present, _ := sensorText(ctx, filepath.Join(dir, "present"))
		if kind != "Battery" || present == "0" {
			continue
		}
		return readBatteryFields(ctx, dir)
	}
	return Battery{}, ErrUnsupported
}
func readBatteryFields(ctx context.Context, dir string) (Battery, error) {
	text, err := sensorText(ctx, filepath.Join(dir, "capacity"))
	if err != nil {
		return Battery{}, err
	}
	percent, err := strconv.ParseFloat(text, 64)
	if err != nil || !finite(percent) || percent < 0 || percent > 100 {
		return Battery{}, errors.New("invalid battery charge")
	}
	state, _ := sensorText(ctx, filepath.Join(dir, "status"))
	result := Battery{Percent: percent, State: batteryState(state)}
	result.SecondsRemaining = batteryRemaining(ctx, dir, result.State)
	return result, nil
}
func batteryRemaining(ctx context.Context, dir, state string) uint64 {
	field := "time_to_empty_now"
	if state == BatteryCharging {
		field = "time_to_full_now"
	}
	if text, err := sensorText(ctx, filepath.Join(dir, field)); err == nil {
		seconds, _ := strconv.ParseUint(text, 10, 64)
		if seconds > 0 && seconds <= 7*86400 {
			return seconds
		}
	}
	if state != BatteryDischarging {
		return 0
	}
	energy, _ := sensorText(ctx, filepath.Join(dir, "energy_now"))
	power, _ := sensorText(ctx, filepath.Join(dir, "power_now"))
	e, _ := strconv.ParseFloat(energy, 64)
	p, _ := strconv.ParseFloat(power, 64)
	if !finite(e) || !finite(p) || e <= 0 || p <= 0 {
		return 0
	}
	seconds := e / p * 3600
	if !finite(seconds) || seconds > 7*86400 {
		return 0
	}
	return uint64(seconds)
}
func batteryState(state string) string {
	switch state {
	case "Charging":
		return BatteryCharging
	case "Discharging":
		return BatteryDischarging
	case "Full":
		return BatteryFull
	default:
		return BatteryAC
	}
}

func hottestLegacyCPU(legacy Temperature, values []ComponentTemperature, name string) Temperature {
	for _, reading := range values {
		if reading.Kind != KindCPU {
			continue
		}
		if legacy.Sensor == "" || reading.Celsius > legacy.Celsius {
			legacy = Temperature{Sensor: name, Celsius: reading.Celsius}
		}
	}
	return legacy
}
