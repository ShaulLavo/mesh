//go:build !linux && !darwin

package worker

import (
	"fmt"
	"net"
)

func (w *Worker) validateAgentCaller(net.Conn, int) error {
	return fmt.Errorf("worker: automatic agent capture is unavailable on this platform")
}
