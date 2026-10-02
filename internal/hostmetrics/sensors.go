package hostmetrics

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const maximumSensors = 64

// A kernel sensor read may ignore cancellation; at most one remains in flight.
func (c *systemCollector) Temperature(ctx context.Context) (Temperature, error) {
	if err := ctx.Err(); err != nil {
		return Temperature{}, fmt.Errorf("CPU temperature: %w", err)
	}
	c.sensorMu.Lock()
	if c.sensorBusy {
		c.sensorMu.Unlock()
		return Temperature{}, errors.New("CPU sensor read is still in progress")
	}
	c.sensorBusy = true
	c.sensorMu.Unlock()
	result := make(chan temperatureResult, 1)
	go c.collectTemperature(ctx, result)
	select {
	case <-ctx.Done():
		return Temperature{}, fmt.Errorf("CPU temperature: %w", ctx.Err())
	case value := <-result:
		if err := ctx.Err(); err != nil {
			return Temperature{}, fmt.Errorf("CPU temperature: %w", err)
		}
		return value.temperature, value.err
	}
}

type temperatureResult struct {
	temperature Temperature
	err         error
}

func (c *systemCollector) collectTemperature(ctx context.Context, result chan<- temperatureResult) {
	root := c.temperatureRoot
	if root == "" {
		root = "/sys"
	}
	components, value, err := readComponentSensors(ctx, root)
	legacyErr := err
	if legacyErr == nil && value.Sensor == "" {
		legacyErr = ErrUnsupported
	}
	c.sensorMu.Lock()
	c.sensorBusy = false
	if ctx.Err() == nil {
		c.components = components
		c.componentsErr = err
		c.componentsAt = time.Now()
	}
	c.sensorMu.Unlock()
	// Cancelled callers discard late values, so they cannot become fresh samples.
	result <- temperatureResult{temperature: value, err: legacyErr}
}
