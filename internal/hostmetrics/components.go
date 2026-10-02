package hostmetrics

import (
	"context"
	"fmt"
	"time"
)

func (c *systemCollector) Components(ctx context.Context) ([]ComponentTemperature, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("component sensors: %w", err)
	}
	c.sensorMu.Lock()
	defer c.sensorMu.Unlock()
	result := append([]ComponentTemperature(nil), c.components...)
	for i := range result {
		result[i].AgeMillis = max(0, time.Since(c.componentsAt).Milliseconds())
	}
	if c.gpuTemperatureAt.IsZero() {
		return result, c.componentsErr
	}
	result = append(result, ComponentTemperature{Kind: KindGPU, Label: "GPU", Celsius: c.gpuTemperature, AgeMillis: max(0, time.Since(c.gpuTemperatureAt).Milliseconds())})
	return HottestTemperatures(result), nil
}
