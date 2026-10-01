//go:build !linux

package hostmetrics

import (
	"context"
	"fmt"
)

func (systemCollector) Temperature(ctx context.Context) (Temperature, error) {
	if err := ctx.Err(); err != nil {
		return Temperature{}, fmt.Errorf("CPU temperature context: %w", err)
	}
	return Temperature{}, ErrUnsupported
}
