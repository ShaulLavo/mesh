//go:build linux

package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maximumSensors = 64

func (systemCollector) Temperature(ctx context.Context) (Temperature, error) {
	return readTemperature(ctx, "/sys")
}
func readTemperature(ctx context.Context, root string) (Temperature, error) {
	var failures []error
	thermal, err := sensorDirs(filepath.Join(root, "class/thermal"))
	if err != nil && !errors.Is(err, ErrUnsupported) {
		failures = append(failures, err)
	}
	value, thermalErr := thermalTemperature(ctx, root, thermal)
	if thermalErr == nil {
		return value, nil
	}
	if !errors.Is(thermalErr, ErrUnsupported) {
		failures = append(failures, thermalErr)
	}
	hwmon, err := sensorDirs(filepath.Join(root, "class/hwmon"))
	if err != nil && !errors.Is(err, ErrUnsupported) {
		failures = append(failures, err)
	}
	for _, entry := range hwmon {
		dir := filepath.Join(root, "class/hwmon", entry.Name())
		value, err := packageTemperature(ctx, dir)
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, ErrUnsupported) {
			failures = append(failures, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return Temperature{}, fmt.Errorf("CPU temperature: %w", err)
	}
	if len(failures) > 0 {
		return Temperature{}, errors.Join(failures...)
	}
	return Temperature{}, ErrUnsupported
}
func packageTemperature(ctx context.Context, dir string) (Temperature, error) {
	name, err := sensorText(ctx, filepath.Join(dir, "name"))
	if err != nil || (name != "coretemp" && name != "k10temp" && name != "zenpower") {
		return Temperature{}, ErrUnsupported
	}
	var failure error
	for i := 1; i <= maximumSensors; i++ {
		prefix := filepath.Join(dir, fmt.Sprintf("temp%d", i))
		label, err := sensorText(ctx, prefix+"_label")
		if err != nil || !packageSensor(label) {
			continue
		}
		value, err := sensorValue(ctx, name+": "+label, prefix+"_input")
		if err == nil {
			return value, nil
		}
		failure = err
	}
	if failure != nil {
		return Temperature{}, failure
	}
	return Temperature{}, ErrUnsupported
}
func cpuSensor(name string) bool {
	switch strings.ToLower(name) {
	case "cpu-thermal", "cpu_thermal", "x86_pkg_temp", "bcm2835_thermal":
		return true
	}
	return false
}
func packageSensor(label string) bool {
	label = strings.ToLower(label)
	return strings.HasPrefix(label, "package id ") || label == "tctl" || label == "tdie"
}
func sensorDirs(root string) ([]os.DirEntry, error) {
	f, err := os.Open(root) // #nosec G304 -- fixed sysfs collector root, replaced only by tests.
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, fmt.Errorf("sensor directories: %w", err)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(maximumSensors)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("sensor directories: %w", err)
	}
	return entries, nil
}
func sensorText(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("sensor text: %w", err)
	}
	f, err := os.Open(path) // #nosec G304 -- paths are bounded sysfs sensor fields.
	if err != nil {
		return "", fmt.Errorf("sensor text: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", fmt.Errorf("sensor text: %w", err)
	}
	if len(data) > 256 {
		return "", errors.New("sensor read exceeds 256 bytes")
	}
	return strings.TrimSpace(string(data)), nil
}
func sensorValue(ctx context.Context, name, path string) (Temperature, error) {
	text, err := sensorText(ctx, path)
	if err != nil {
		return Temperature{}, fmt.Errorf("CPU temperature: %w", err)
	}
	milli, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return Temperature{}, fmt.Errorf("CPU temperature: %w", err)
	}
	v := milli / 1000
	if !finite(v) || v < -273.15 || v > 1000 {
		return Temperature{}, errors.New("invalid CPU sensor temperature")
	}
	return Temperature{Sensor: name, Celsius: v}, nil
}

func thermalTemperature(ctx context.Context, root string, entries []os.DirEntry) (Temperature, error) {
	var failure error
	for _, entry := range entries {
		dir := filepath.Join(root, "class/thermal", entry.Name())
		name, err := sensorText(ctx, filepath.Join(dir, "type"))
		if err != nil || !cpuSensor(name) {
			continue
		}
		value, err := sensorValue(ctx, name, filepath.Join(dir, "temp"))
		if err == nil {
			return value, nil
		}
		failure = err
	}
	if failure != nil {
		return Temperature{}, failure
	}
	return Temperature{}, ErrUnsupported
}
