//go:build linux

package hostmetrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTemperatureReviewID7BlockingFileCancellation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "class/thermal/thermal_zone0")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte("cpu-thermal"), 0600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "temp")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(fifo, os.O_RDWR, 0600) //nolint:gosec // the FIFO is inside this test's owned fake sysfs root
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	collector := &systemCollector{temperatureRoot: root}
	done := make(chan error, 1)
	go func() { _, err := collector.Temperature(ctx); done <- err }()
	defer func() {
		_, _ = holder.Write([]byte("42500"))
		_ = holder.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned sensor read did not release")
		}
	}()
	deadline := time.Now().Add(time.Second)
	for !reviewSensorOpen(fifo) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !reviewSensorOpen(fifo) {
		t.Fatal("known blocking read was not opened")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		done <- err
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancelled sensor read retained the collector")
	}
	for range 20 {
		if _, err := collector.Temperature(t.Context()); err == nil {
			t.Fatal("blocked sensor started another read")
		}
	}
	if countReviewSensors(fifo) != 2 {
		t.Fatal("blocked sensor workers accumulated")
	}
	sampler := NewWithCollector(collector, time.Now)
	nextCtx, nextCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer nextCancel()
	value, err := sampler.Read(nextCtx)
	if err != nil || value.RAM.Availability != Available || !value.Temperature.Failing {
		t.Fatalf("blocked sensor stalled independent RAM reading: %+v %v", value, err)
	}
	release := sampler.Demand(MinimumInterval)
	defer release()
	running, stop := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	samples := make(chan Snapshot, 4)
	go func() { defer close(stopped); sampler.Run(running, func(s Snapshot) { samples <- s }) }()
	defer func() { stop(); <-stopped }()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case s := <-samples:
			if s.CPU.Availability == Available && s.RAM.Availability == Available {
				stop()
				select {
				case <-stopped:
					return
				case <-time.After(200 * time.Millisecond):
					t.Fatal("sensor retained sampler shutdown")
				}
			}
		case <-timeout.C:
			t.Fatal("blocked sensor starved independent CPU/RAM")
		}
	}
}
func reviewSensorOpen(path string) bool { return countReviewSensors(path) >= 2 }
func countReviewSensors(path string) int {
	entries, _ := os.ReadDir("/proc/self/fd")
	count := 0
	for _, entry := range entries {
		target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if target == path {
			count++
		}
	}
	return count
}

func TestTemperatureReviewID7LateValueKeepsLastGoodAge(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "class/thermal/thermal_zone0")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte("cpu-thermal"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "temp")
	if err := os.WriteFile(path, []byte("42500"), 0600); err != nil {
		t.Fatal(err)
	}
	collector := &systemCollector{temperatureRoot: root}
	var offset atomic.Int64
	sampler := NewWithCollector(collector, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
	before, err := sampler.Read(t.Context())
	if err != nil || before.Temperature.Availability != Available {
		t.Fatal(before, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(path, os.O_RDWR, 0600) //nolint:gosec // the FIFO is inside this test's owned fake sysfs root
	if err != nil {
		t.Fatal(err)
	}
	offset.Add(int64(TemperatureInterval))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := sampler.Read(ctx); done <- err }()
	defer func() { _, _ = holder.Write([]byte("99000")); _ = holder.Close() }()
	deadline := time.Now().Add(time.Second)
	for !reviewSensorOpen(path) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !reviewSensorOpen(path) {
		t.Fatal("sampler did not reach blocked sensor")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("sensor retained shared sampler gate")
	}
	value, err := sampler.Read(t.Context())
	if err != nil || value.RAM.Availability != Available || !value.Temperature.Failing || value.Temperature.Sample != before.Temperature.Sample || value.Temperature.AgeMillis < 10000 {
		t.Fatal("cancellation lost independent or aged last-good readings", value, err)
	}
	_, _ = holder.Write([]byte("99000"))
	_ = holder.Close()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		collector.sensorMu.Lock()
		busy := collector.sensorBusy
		collector.sensorMu.Unlock()
		if !busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	late, err := sampler.Read(t.Context())
	if err != nil || late.Temperature.Value.Celsius != 42.5 || late.Temperature.Sample != before.Temperature.Sample || late.Temperature.AgeMillis < value.Temperature.AgeMillis {
		t.Fatal("late sensor completion became a fresh observation", late, err)
	}
}
