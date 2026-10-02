//go:build darwin

package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	gnet "github.com/shirou/gopsutil/v4/net"
)

func readComponentSensors(ctx context.Context, _ string) ([]ComponentTemperature, Temperature, error) {
	if err := ctx.Err(); err != nil {
		return nil, Temperature{}, err
	}
	if runtime.GOARCH != "arm64" {
		return nil, Temperature{}, ErrUnsupported
	}
	n, err := nativeAPI()
	if err != nil {
		return nil, Temperature{}, err
	}
	if n.hidError != nil {
		return nil, Temperature{}, n.hidError
	}
	client := n.hidCreate(0)
	if client == 0 {
		return nil, Temperature{}, errors.New("IOHID sensor client unavailable")
	}
	defer n.release(client)
	match := n.dictionaryCreate(0, 2, 0, 0)
	if match == 0 {
		return nil, Temperature{}, errors.New("IOHID matching dictionary unavailable")
	}
	defer n.release(match)
	page, usage := int64(0xff00), int64(5)
	pageValue := n.numberCreate(0, 4, &page)
	usageValue := n.numberCreate(0, 4, &usage)
	pageKey := n.stringCreate(0, "PrimaryUsagePage", cfUTF8)
	usageKey := n.stringCreate(0, "PrimaryUsage", cfUTF8)
	defer func() {
		for _, value := range []uintptr{pageValue, usageValue, pageKey, usageKey} {
			if value != 0 {
				n.release(value)
			}
		}
	}()
	if pageValue == 0 || usageValue == 0 || pageKey == 0 || usageKey == 0 {
		return nil, Temperature{}, errors.New("IOHID matching values unavailable")
	}
	n.dictionarySet(match, pageKey, pageValue)
	n.dictionarySet(match, usageKey, usageValue)
	n.hidMatching(client, match)
	services := n.hidServices(client)
	if services == 0 {
		return nil, Temperature{}, ErrUnsupported
	}
	defer n.release(services)
	count := n.arrayCount(services)
	if count < 0 || count > 256 {
		return nil, Temperature{}, errors.New("IOHID sensor count exceeds bound")
	}
	product := n.stringCreate(0, "Product", cfUTF8)
	if product == 0 {
		return nil, Temperature{}, errors.New("IOHID product key unavailable")
	}
	defer n.release(product)
	var values []ComponentTemperature
	for i := int64(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, Temperature{}, err
		}
		service := n.arrayValue(services, i)
		if service == 0 {
			continue
		}
		nameValue := n.hidProperty(service, product)
		if nameValue == 0 {
			continue
		}
		name := n.text(nameValue)
		n.release(nameValue)
		kind := appleSensorKind(name)
		if kind == "" {
			continue
		}
		event := n.hidEvent(service, 15, 0, 0)
		if event == 0 {
			continue
		}
		celsius := n.hidValue(event, 15<<16)
		n.release(event)
		if !finite(celsius) || celsius <= 0 || celsius > 150 {
			continue
		}
		values = append(values, ComponentTemperature{Kind: kind, Celsius: celsius})
	}
	values = HottestTemperatures(values)
	if len(values) == 0 {
		return nil, Temperature{}, ErrUnsupported
	}
	var legacy Temperature
	for _, value := range values {
		if value.Kind == KindCPU {
			legacy = Temperature{Sensor: "Apple CPU die", Celsius: value.Celsius}
		}
	}
	return values, legacy, nil
}
func appleSensorKind(name string) string {
	if strings.HasPrefix(name, "pACC MTR Temp") || strings.HasPrefix(name, "eACC MTR Temp") {
		return KindCPU
	}
	if strings.HasPrefix(name, "GPU MTR Temp") {
		return KindGPU
	}
	return ""
}
func (*systemCollector) GPU(ctx context.Context) (GPU, error) {
	if err := ctx.Err(); err != nil {
		return GPU{}, err
	}
	n, err := nativeAPI()
	if err != nil {
		return GPU{}, err
	}
	var result GPU
	found := false
	err = n.services("IOAccelerator", func(service uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats := n.field(service, "PerformanceStatistics")
		if stats == 0 {
			return nil
		}
		defer n.release(stats)
		value, ok := n.number(n.get(stats, "Device Utilization %"))
		if !ok {
			value, ok = n.number(n.get(stats, "GPU Activity(%)"))
		}
		if !ok {
			return nil
		}
		if value < 0 || value > 100 {
			return errors.New("invalid IOAccelerator utilization")
		}
		found = true
		result.Utilization = max(result.Utilization, value)
		// Apple Silicon reports allocated unified memory; it has no dedicated VRAM capacity.
		if memory, ok := n.number(n.get(stats, "In use system memory")); ok && memory >= 0 && memory < 1<<50 {
			result.MemoryUsedBytes += uint64(memory)
			result.MemoryKind = "shared"
		}
		return nil
	})
	if err != nil {
		return GPU{}, err
	}
	if !found {
		return GPU{}, ErrUnsupported
	}
	return result, nil
}
func (*systemCollector) Battery(ctx context.Context) (Battery, error) {
	if err := ctx.Err(); err != nil {
		return Battery{}, err
	}
	n, err := nativeAPI()
	if err != nil {
		return Battery{}, err
	}
	var result Battery
	err = n.services("AppleSmartBattery", func(service uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, ok := n.fieldNumber(service, "CurrentCapacity")
		if !ok {
			return errors.New("native battery charge unavailable")
		}
		capacity, ok := n.fieldNumber(service, "MaxCapacity")
		if !ok || capacity <= 0 || current < 0 || current > capacity {
			return errors.New("invalid native battery capacity")
		}
		result.Percent = current / capacity * 100
		result.State = BatteryDischarging
		if n.fieldBool(service, "ExternalConnected") {
			result.State = BatteryAC
		}
		if n.fieldBool(service, "IsCharging") {
			result.State = BatteryCharging
		}
		if n.fieldBool(service, "FullyCharged") {
			result.State = BatteryFull
		}
		remaining, ok := n.fieldNumber(service, "TimeRemaining")
		if ok && remaining > 0 && remaining < 65535 && remaining*60 <= 7*86400 && (result.State == BatteryCharging || result.State == BatteryDischarging) {
			result.SecondsRemaining = uint64(remaining * 60)
		}
		return nil
	})
	return result, err
}
func (*systemCollector) Disk(ctx context.Context) (DiskCounters, error) {
	if err := ctx.Err(); err != nil {
		return DiskCounters{}, err
	}
	values, err := disk.IOCountersWithContext(ctx)
	if err != nil {
		return DiskCounters{}, fmt.Errorf("native disk counters: %w", err)
	}
	var result DiskCounters
	var devices []string
	for name, value := range values {
		if !addCounter(&result.ReadBytes, value.ReadBytes) || !addCounter(&result.WriteBytes, value.WriteBytes) {
			return DiskCounters{}, errors.New("native disk counter sum exceeds bound")
		}
		devices = append(devices, name)
	}
	if len(devices) == 0 {
		return DiskCounters{}, ErrUnsupported
	}
	sort.Strings(devices)
	result.Devices = strings.Join(devices, ",")
	return result, nil
}
func (*systemCollector) Network(ctx context.Context) (NetworkCounters, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	values, err := gnet.IOCountersWithContext(ctx, true)
	if err != nil {
		return NetworkCounters{}, fmt.Errorf("native network counters: %w", err)
	}
	var result NetworkCounters
	var devices []string
	for _, value := range values {
		if !realInterface(value.Name) {
			continue
		}
		if !addCounter(&result.ReceiveBytes, value.BytesRecv) || !addCounter(&result.SendBytes, value.BytesSent) {
			return NetworkCounters{}, errors.New("native network counter sum exceeds bound")
		}
		devices = append(devices, value.Name)
	}
	if len(devices) == 0 {
		return NetworkCounters{}, ErrUnsupported
	}
	sort.Strings(devices)
	result.Devices = strings.Join(devices, ",")
	return result, nil
}
