//go:build darwin

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/worker"
)

func (m mcpLauncher) CheckPortFree(ctx context.Context, targets []netip.AddrPort) error {
	checked := map[uint16]bool{}
	for _, target := range targets {
		port := target.Port()
		if checked[port] {
			continue
		}
		checked[port] = true
		listeners, err := darwinPortListeners(ctx, port)
		if err != nil {
			return err
		}
		if len(listeners) > 0 {
			return fmt.Errorf("managed HTTP endpoint %s is already occupied by another listener", target)
		}
	}
	return nil
}

func (m mcpLauncher) VerifyManagedDial(ctx context.Context, ref mcphost.WorkerRef, target netip.AddrPort, dial func() (net.Conn, error)) (net.Conn, error) {
	leaderPID, err := m.liveWorkerLeaderPIDDarwin(ctx, ref)
	if err != nil {
		return nil, err
	}
	ownerBefore, err := verifyOwnedListenerDarwin(ctx, ref.SessionID, leaderPID, target.Port())
	if err != nil {
		return nil, err
	}
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	ownerAfter, err := verifyOwnedListenerDarwin(ctx, ref.SessionID, leaderPID, target.Port())
	if err != nil || ownerAfter != ownerBefore {
		_ = conn.Close()
		if err != nil && errors.Is(err, mcphost.ErrManagedListenerMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: listener changed during dial to %s", mcphost.ErrManagedListenerMismatch, target)
	}
	return conn, nil
}

func (m mcpLauncher) liveWorkerLeaderPIDDarwin(ctx context.Context, ref mcphost.WorkerRef) (int, error) {
	journal, b, err := m.binding(ref)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", mcphost.ErrManagedListenerMismatch, err)
	}
	r, err := journal.Exact(ctx, b)
	if err != nil || r.Phase == executionTerminalPhase || b.Reservation.BootID != worker.BootID() {
		return 0, fmt.Errorf("%w: managed worker %s is no longer live", mcphost.ErrManagedListenerMismatch, ref.SessionID)
	}
	meta, err := worker.ReadMeta(filepath.Join(m.l.sessionsDir, ref.SessionID))
	if err != nil || meta.Reservation == nil || *meta.Reservation != b.Reservation || meta.PID <= 1 {
		return 0, fmt.Errorf("%w: managed worker %s metadata mismatch", mcphost.ErrManagedListenerMismatch, ref.SessionID)
	}
	if meta.State == worker.StateExited || meta.State == worker.StateInterrupted {
		return 0, fmt.Errorf("%w: managed worker %s has exited", mcphost.ErrManagedListenerMismatch, ref.SessionID)
	}
	return meta.PID, nil
}

func verifyOwnedListenerDarwin(ctx context.Context, sessionID string, leaderPID int, port uint16) (int, error) {
	listeners, err := darwinPortListeners(ctx, port)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", mcphost.ErrManagedListenerMismatch, err)
	}
	if len(listeners) == 0 {
		return 0, mcphost.ErrManagedListenerNotReady
	}
	allowed, err := darwinProcessSubtree(ctx, leaderPID)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", mcphost.ErrManagedListenerMismatch, err)
	}
	if !allowed[leaderPID] {
		return 0, fmt.Errorf("%w: worker %s leader pid %d exited", mcphost.ErrManagedListenerMismatch, sessionID, leaderPID)
	}
	ownerPID := 0
	for _, pid := range listeners {
		if !allowed[pid] {
			return 0, fmt.Errorf("%w: port %d is held by pid %d outside worker %s", mcphost.ErrManagedListenerMismatch, port, pid, sessionID)
		}
		if ownerPID == 0 {
			ownerPID = pid
		}
	}
	return ownerPID, nil
}

func darwinPortListeners(ctx context.Context, port uint16) ([]int, error) {
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "/usr/sbin/lsof", "-nP", "-iTCP:"+strconv.Itoa(int(port)), "-sTCP:LISTEN", "-Fp").Output() //nolint:gosec // fixed system tool with numeric port
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(out) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect darwin listeners on port %d: %w", port, err)
	}
	var pids []int
	for line := range strings.SplitSeq(string(out), "\n") {
		if raw, ok := strings.CutPrefix(line, "p"); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
	}
	return pids, nil
}

func darwinProcessSubtree(ctx context.Context, leaderPID int) (map[int]bool, error) {
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "/bin/ps", "-e", "-o", "pid=,ppid=,pgid=").Output() //nolint:gosec // fixed system tool with constant flags
	if err != nil {
		return nil, fmt.Errorf("list darwin processes: %w", err)
	}
	children := map[int][]int{}
	present := map[int]bool{}
	pgidMatch := map[int]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil || pid <= 0 {
			continue
		}
		present[pid] = true
		children[ppid] = append(children[ppid], pid)
		if pgid == leaderPID {
			pgidMatch[pid] = true
		}
	}
	allowed := map[int]bool{}
	if !present[leaderPID] {
		return allowed, nil
	}
	queue := []int{leaderPID}
	allowed[leaderPID] = true
	for pid := range pgidMatch {
		if !allowed[pid] {
			allowed[pid] = true
			queue = append(queue, pid)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range children[cur] {
			if !allowed[child] {
				allowed[child] = true
				queue = append(queue, child)
			}
		}
	}
	return allowed, nil
}
