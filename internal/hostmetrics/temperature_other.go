//go:build !linux

package hostmetrics

import "context"

func (systemCollector) Temperature(ctx context.Context) (Temperature, error) {
	if err := ctx.Err(); err != nil {
		return Temperature{}, err
	}
	return Temperature{}, ErrUnsupported
}
