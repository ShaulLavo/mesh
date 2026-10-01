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

const maxSensors = 64

func (systemCollector) Temperature(ctx context.Context) (Temperature, error) {
	return readTemperature(ctx, "/sys")
}

func readTemperature(ctx context.Context, sys string) (Temperature, error) {
	if err := ctx.Err(); err != nil {
		return Temperature{}, err
	}
	value, err := thermalTemperature(ctx, filepath.Join(sys, "class", "thermal"))
	if err == nil || !errors.Is(err, ErrUnsupported) {
		return value, err
	}
	return hwmonTemperature(ctx, filepath.Join(sys, "class", "hwmon"))
}

func thermalTemperature(ctx context.Context, root string) (Temperature, error) {
	entries, err := sensorDirectories(root)
	if err != nil {
		return Temperature{}, err
	}
	for _, entry := range entries {
		value, err := readThermalSensor(ctx, filepath.Join(root, entry.Name()))
		if !errors.Is(err, ErrUnsupported) {
			return value, err
		}
	}
	return Temperature{}, ErrUnsupported
}

func readThermalSensor(ctx context.Context, dir string) (Temperature, error) {
	name, err := sensorText(ctx, filepath.Join(dir, "type"))
	if err != nil || !cpuSensor(name) {
		return Temperature{}, ErrUnsupported
	}
	return sensorValue(ctx, name, filepath.Join(dir, "temp"))
}

func hwmonTemperature(ctx context.Context, root string) (Temperature, error) {
	entries, err := sensorDirectories(root)
	if err != nil {
		return Temperature{}, err
	}
	for _, entry := range entries {
		value, err := readHwmonSensor(ctx, filepath.Join(root, entry.Name()))
		if !errors.Is(err, ErrUnsupported) {
			return value, err
		}
	}
	return Temperature{}, ErrUnsupported
}

func readHwmonSensor(ctx context.Context, dir string) (Temperature, error) {
	name, err := sensorText(ctx, filepath.Join(dir, "name"))
	if err != nil || (name != "coretemp" && name != "k10temp" && name != "zenpower") {
		return Temperature{}, ErrUnsupported
	}
	for index := 1; index <= maxSensors; index++ {
		value, err := readPackageSensor(ctx, dir, name, index)
		if !errors.Is(err, ErrUnsupported) {
			return value, err
		}
	}
	return Temperature{}, ErrUnsupported
}

func readPackageSensor(ctx context.Context, dir, name string, index int) (Temperature, error) {
	prefix := filepath.Join(dir, fmt.Sprintf("temp%d", index))
	label, err := sensorText(ctx, prefix+"_label")
	if err != nil || !packageSensor(label) {
		return Temperature{}, ErrUnsupported
	}
	return sensorValue(ctx, name+": "+label, prefix+"_input")
}

func cpuSensor(name string) bool {
	switch strings.ToLower(name) {
	case "cpu-thermal", "cpu_thermal", "x86_pkg_temp", "soc_thermal", "bcm2835_thermal":
		return true
	default:
		return false
	}
}

func packageSensor(label string) bool {
	label = strings.ToLower(label)
	return strings.HasPrefix(label, "package id ") || label == "tctl" || label == "tdie"
}

func sensorDirectories(root string) ([]os.DirEntry, error) {
	f, err := os.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxSensors)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return entries, err
}

func sensorText(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", err
	}
	if len(data) > 256 {
		return "", errors.New("sensor reading exceeds limit")
	}
	return strings.TrimSpace(string(data)), nil
}

func sensorValue(ctx context.Context, name, path string) (Temperature, error) {
	text, err := sensorText(ctx, path)
	if err != nil {
		return Temperature{}, err
	}
	milli, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return Temperature{}, fmt.Errorf("sensor %s: %w", name, err)
	}
	return Temperature{Sensor: name, Celsius: milli / 1000}, nil
}
