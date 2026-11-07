package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

// ExecutionEnvironment supplies exactly the snapshot later inherited by this attempt.
func ExecutionEnvironment(env []string, host string, ref session.Reservation, term string) []string {
	out := make([]string, 0, len(env)+3)
	for _, entry := range env {
		if !strings.HasPrefix(entry, MeshDepthVariable+"=") {
			out = append(out, entry)
		}
	}
	return withSessionIdentity(withTerm(out, term), host, ref.ID)
}
func prepareExecution(cfg Config) (*execution.Attempt, error) {
	if cfg.Reservation != nil {
		if cfg.Reservation.ID != cfg.ID {
			return nil, fmt.Errorf("worker: reservation ID mismatch")
		}
		if err := validateReservation(filepath.Dir(cfg.Dir), *cfg.Reservation); err != nil {
			return nil, err
		}
	}
	if cfg.Execution == nil {
		if cfg.Reservation != nil && cfg.Reservation.Purpose == session.Execution {
			return nil, fmt.Errorf("worker: missing execution binding")
		}
		return nil, nil
	}
	if cfg.Reservation == nil || *cfg.Reservation != cfg.Execution.Reservation {
		return nil, fmt.Errorf("worker: execution reference mismatch")
	}
	intent, err := execution.Normalize(execution.Intent{Kind: "pty", Command: cfg.Command, Cwd: cfg.Cwd, Cols: cfg.Cols, Rows: cfg.Rows, Term: executionTerminal(cfg.Env)})
	if err != nil {
		return nil, workerExecutionError("execution.go prepareExecution", err)
	}
	executionValue1_0, executionValue1_1 := execution.StartAttempt(context.Background(), filepath.Dir(filepath.Dir(cfg.Dir)), *cfg.Execution, intent, cfg.Env)
	return executionValue1_0, workerExecutionError("execution.go prepareExecution", executionValue1_1)
}
func executionTermination(state *os.ProcessState, code int) execution.Termination {
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal := int(status.Signal())
			return execution.Termination{Kind: "signal", Signal: &signal}
		}
	}
	if code < 0 {
		return execution.Termination{Kind: "interrupted", Reason: "wait_failed"}
	}
	return execution.Termination{Kind: "exit", Code: &code}
}

func workerExecutionError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func executionTerminal(env []string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, "TERM=") {
			return strings.TrimPrefix(entry, "TERM=")
		}
	}
	return "dumb"
}

// All keyed connection handlers join before the attempt releases pathname ownership.
type executionClients struct {
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	joined      sync.WaitGroup
	stop        sync.Once
}

func newExecutionClients() *executionClients {
	return &executionClients{connections: make(map[net.Conn]struct{})}
}
func (c *executionClients) serve(w *Worker, conn net.Conn) {
	c.mu.Lock()
	c.connections[conn] = struct{}{}
	c.joined.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.joined.Done()
		defer func() { c.mu.Lock(); delete(c.connections, conn); c.mu.Unlock() }()
		w.serve(conn)
	}()
}
func (w *Worker) stopExecutionServing(ln net.Listener, done <-chan struct{}) {
	w.executionClients.stop.Do(func() {
		_ = ln.Close()
		<-done
		w.executionClients.closeConnections()
		w.executionClients.joined.Wait()
	})
}
func (c *executionClients) closeConnections() {
	c.mu.Lock()
	connections := make([]net.Conn, 0, len(c.connections))
	for conn := range c.connections {
		connections = append(connections, conn)
	}
	c.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}
