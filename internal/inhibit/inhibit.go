// Package inhibit keeps the host awake while the daemon has live workers.
package inhibit

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
)

const releaseTimeout = time.Second

// Manager holds one inhibitor regardless of the number of live workers.
// Unavailable mechanisms are reported once and disabled for this daemon run.
type Manager struct {
	mu       sync.Mutex
	command  func() (*exec.Cmd, error)
	report   func(error)
	timeout  time.Duration
	lease    *processLease
	disabled bool
	closed   bool
}

func New(report func(error)) *Manager {
	if report == nil {
		report = func(err error) { log.Print(err) }
	}
	return &Manager{
		command: func() (*exec.Cmd, error) { return platformCommand(runtime.GOOS) },
		report:  report,
		timeout: releaseTimeout,
	}
}

// Update follows complete worker observations. Repeated active updates also
// detect a utility that started successfully but could not acquire its lock.
func (m *Manager) Update(active bool) {
	m.mu.Lock()
	err := m.update(active)
	m.mu.Unlock()
	if err != nil {
		m.report(fmt.Errorf("sleep inhibition unavailable; continuing without it: %w", err))
	}
}

func (m *Manager) update(active bool) error {
	if m.closed || m.disabled {
		return nil
	}
	if m.lease != nil && m.lease.finished() {
		err := m.lease.exitError()
		_ = m.lease.input.Close()
		m.lease = nil
		m.disabled = true
		return err
	}
	if active && m.lease == nil {
		lease, err := startLease(m.command)
		m.lease = lease
		m.disabled = err != nil
		return err
	}
	if active || m.lease == nil {
		return nil
	}
	err := m.lease.close(m.timeout)
	m.lease = nil
	m.disabled = err != nil
	return err
}

// Close releases and reaps the utility. A closed manager cannot acquire again.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.lease == nil {
		return nil
	}
	err := m.lease.close(m.timeout)
	m.lease = nil
	return err
}

func platformCommand(goos string) (*exec.Cmd, error) {
	switch goos {
	case "linux":
		return exec.Command("systemd-inhibit", "--no-ask-password", "--what=sleep:idle", "--mode=block", "--who=mesh", "--why=Mesh sessions are running", "/bin/cat"), nil
	case "darwin":
		return exec.Command("/usr/bin/caffeinate", "-i", "/bin/cat"), nil
	default:
		return nil, fmt.Errorf("no sleep inhibitor for %s", goos)
	}
}

type processLease struct {
	command *exec.Cmd
	input   *os.File
	done    chan struct{}
	err     error
}

func startLease(command func() (*exec.Cmd, error)) (*processLease, error) {
	cmd, err := command()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create inhibitor lifetime pipe: %w", err)
	}
	defer func() { _ = reader.Close() }()
	// os.Pipe marks both ends close-on-exec. Only stdin's read end is
	// inherited, so killing the daemon closes the last writer and ends cat.
	cmd.Stdin = reader
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("start sleep inhibitor: %w", err)
	}
	lease := &processLease{command: cmd, input: writer, done: make(chan struct{})}
	go func() {
		lease.err = cmd.Wait()
		close(lease.done)
	}()
	return lease, nil
}

func (l *processLease) finished() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (l *processLease) exitError() error {
	if l.err != nil {
		return fmt.Errorf("%s exited: %w", l.command.Path, l.err)
	}
	return fmt.Errorf("%s exited before release", l.command.Path)
}

func (l *processLease) close(timeout time.Duration) error {
	_ = l.input.Close()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-l.done:
		return nil
	case <-timer.C:
	}
	// The utility may have forked its command; killing only the immediate
	// child can leave its inhibitor or its pipe reader behind.
	killErr := syscall.Kill(-l.command.Process.Pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	timer.Reset(timeout)
	select {
	case <-l.done:
		return errors.Join(errors.New("sleep inhibitor did not exit after its lifetime pipe closed"), killErr)
	case <-timer.C:
		return errors.Join(errors.New("sleep inhibitor did not exit after SIGKILL"), killErr)
	}
}
