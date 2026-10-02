//go:build !linux && !darwin

package hostmetrics

import "context"

func readComponentSensors(ctx context.Context, _ string) ([]ComponentTemperature, Temperature, error) {
	return nil, Temperature{}, ErrUnsupported
}
func (*systemCollector) GPU(context.Context) (GPU, error) { return GPU{}, ErrUnsupported }
func (*systemCollector) Disk(context.Context) (DiskCounters, error) {
	return DiskCounters{}, ErrUnsupported
}
func (*systemCollector) Network(context.Context) (NetworkCounters, error) {
	return NetworkCounters{}, ErrUnsupported
}
func (*systemCollector) Battery(context.Context) (Battery, error) { return Battery{}, ErrUnsupported }
