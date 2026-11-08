package worker

import (
	"fmt"
	"os"
	"syscall"
)

func (w *Worker) waitChild() (*os.ProcessState, error) {
	if w.cfg.Execution == nil && childExitSupported() != nil {
		return w.waitLegacyChild()
	}
	if err := observeChildExit(w.cmd.Process.Pid); err != nil {
		return nil, fmt.Errorf("worker: observe child termination: %w", err)
	}
	return w.reapChild()
}
func (w *Worker) reapChild() (*os.ProcessState, error) {
	w.operationMu.Lock()
	defer w.operationMu.Unlock()
	w.mu.Lock()
	w.reaped = true
	w.mu.Unlock()
	state, err := w.cmd.Process.Wait()
	return state, workerExecutionError("reap child", err)
}
func (w *Worker) waitLegacyChild() (*os.ProcessState, error) {
	state, err := w.cmd.Process.Wait()
	w.mu.Lock()
	w.reaped = true
	w.mu.Unlock()
	return state, workerExecutionError("wait child", err)
}
func (w *Worker) deliverSignal(instance, name string) (string, error) {
	w.operationMu.Lock()
	defer w.operationMu.Unlock()
	if instance != "" && (w.cfg.Execution == nil || w.cfg.Execution.Reservation.Instance != instance) {
		return deliveryFailed, fmt.Errorf("worker: execution instance mismatch")
	}
	sig, ok := signals[name]
	if !ok || w.cmd == nil || w.cmd.Process == nil {
		return deliveryFailed, fmt.Errorf("worker: invalid signal target")
	}
	w.mu.Lock()
	retired := w.reaped
	w.mu.Unlock()
	if retired {
		return "already_retired", nil
	}
	err := syscall.Kill(-w.cmd.Process.Pid, sig)
	if err == nil {
		return "delivered", nil
	}
	// Ordinary callers retain the existing direct-child fallback while the identifier is fenced.
	if instance == "" {
		err = w.cmd.Process.Signal(sig)
	}
	return deliveryFailed, workerExecutionError("deliver process group signal", err)
}

const deliveryFailed = "failed"
