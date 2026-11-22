//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/sockdiag"
	"github.com/shaul/mesh/internal/worker"
)

// CheckPortFree verifies through the kernel's TCP listener table that no
// process already listens on any of targets (including wildcard binds).
func (m mcpLauncher) CheckPortFree(ctx context.Context, targets []netip.AddrPort) error {
	checked := map[uint16]bool{}
	for _, target := range targets {
		port := target.Port()
		if checked[port] {
			continue
		}
		checked[port] = true
		listeners, err := sockdiag.TCPListeners(ctx, int(port))
		if err != nil {
			return fmt.Errorf("inspect managed HTTP listeners on port %d: %w", port, err)
		}
		for _, l := range listeners {
			for _, t := range targets {
				if listenerConflictsTarget(l.Address, t) {
					return fmt.Errorf("managed HTTP endpoint %s is already occupied by another listener", t)
				}
			}
		}
	}
	return nil
}

// VerifyManagedDial proves that target is bound exclusively by the exact
// reserved worker ref before dialing and again after connecting, before any
// HTTP request or credential header is written to the socket.
func (m mcpLauncher) VerifyManagedDial(ctx context.Context, ref mcphost.WorkerRef, target netip.AddrPort, dial func() (net.Conn, error)) (net.Conn, error) {
	leaderPID, err := m.liveWorkerLeaderPID(ctx, ref)
	if err != nil {
		return nil, err
	}
	inodeBefore, _, err := verifyOwnedListenerLinux(ctx, ref.SessionID, leaderPID, target)
	if err != nil {
		return nil, err
	}
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	inodeAfter, held, err := verifyOwnedListenerLinux(ctx, ref.SessionID, leaderPID, target)
	if err != nil || inodeAfter != inodeBefore {
		_ = conn.Close()
		if err != nil && errors.Is(err, mcphost.ErrManagedListenerMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: listener changed during dial to %s", mcphost.ErrManagedListenerMismatch, target)
	}
	if err := verifyEstablishedPeerLinux(ctx, ref.SessionID, leaderPID, conn, held); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (m mcpLauncher) liveWorkerLeaderPID(ctx context.Context, ref mcphost.WorkerRef) (int, error) {
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

func verifyOwnedListenerLinux(ctx context.Context, sessionID string, leaderPID int, target netip.AddrPort) (uint32, map[uint32]bool, error) {
	listeners, err := sockdiag.TCPListeners(ctx, int(target.Port()))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: inspect TCP listeners: %w", mcphost.ErrManagedListenerMismatch, err)
	}
	held, err := workerHeldSocketsLinux(sessionID, leaderPID)
	if err != nil {
		return 0, nil, err
	}
	euid := uint32(os.Geteuid()) //nolint:gosec // effective UID is non-negative 32-bit
	var exactInode, wildInode uint32
	for _, l := range listeners {
		if !listenerConflictsTarget(l.Address, target) {
			continue
		}
		if l.UID != euid || !held[l.Inode] {
			return 0, nil, fmt.Errorf("%w: %s is held by a process outside worker %s", mcphost.ErrManagedListenerMismatch, target, sessionID)
		}
		if l.Address.Addr().Unmap() == target.Addr().Unmap() {
			exactInode = l.Inode
		} else if wildInode == 0 {
			wildInode = l.Inode
		}
	}
	if exactInode != 0 {
		return exactInode, held, nil
	}
	if wildInode != 0 {
		return wildInode, held, nil
	}
	return 0, nil, mcphost.ErrManagedListenerNotReady
}

func verifyEstablishedPeerLinux(ctx context.Context, sessionID string, leaderPID int, conn net.Conn, held map[uint32]bool) error {
	remote, okR := conn.RemoteAddr().(*net.TCPAddr)
	local, okL := conn.LocalAddr().(*net.TCPAddr)
	if !okR || !okL {
		return nil
	}
	peer, err := sockdiag.Peer(ctx, remote.AddrPort(), local.AddrPort())
	if err != nil {
		// Before the server calls accept(), the child socket in the accept queue
		// has no inode yet; the before/after listener inode check already proved
		// exclusive ownership of the listening socket.
		return nil //nolint:nilerr // unaccepted peer sockets have no kernel inode yet
	}
	euid := uint32(os.Geteuid()) //nolint:gosec // effective UID is non-negative 32-bit
	if peer.UID != euid {
		return fmt.Errorf("%w: peer socket UID mismatch on %s", mcphost.ErrManagedListenerMismatch, remote)
	}
	if held[peer.Inode] {
		return nil
	}
	// Refresh once in case accept() created the descriptor right after held was collected.
	refreshed, err := workerHeldSocketsLinux(sessionID, leaderPID)
	if err != nil || !refreshed[peer.Inode] {
		return fmt.Errorf("%w: accepted socket does not belong to worker %s", mcphost.ErrManagedListenerMismatch, sessionID)
	}
	return nil
}

func workerHeldSocketsLinux(sessionID string, leaderPID int) (map[uint32]bool, error) {
	pids, err := worker.SessionProcesses(sessionID, leaderPID)
	if err != nil {
		return nil, fmt.Errorf("%w: list worker %s processes: %w", mcphost.ErrManagedListenerMismatch, sessionID, err)
	}
	held := map[uint32]bool{}
	for _, pid := range pids {
		if err := collectProcessSocketsLinux(pid, held); err != nil {
			return nil, fmt.Errorf("%w: %w", mcphost.ErrManagedListenerMismatch, err)
		}
	}
	return held, nil
}

func collectProcessSocketsLinux(pid int, held map[uint32]bool) error {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	file, err := os.Open(dir) //nolint:gosec // fixed /proc descriptor directory for worker session process
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = file.Close() }()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, name := range names {
		target, err := os.Readlink(dir + "/" + name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("readlink %s/%s: %w", dir, name, err)
		}
		raw, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		inode, err := strconv.ParseUint(strings.TrimSuffix(raw, "]"), 10, 32)
		if err == nil {
			held[uint32(inode)] = true
		}
	}
	return nil
}

func listenerConflictsTarget(listenAddr, target netip.AddrPort) bool {
	if listenAddr.Port() != target.Port() {
		return false
	}
	la := listenAddr.Addr().Unmap()
	ta := target.Addr().Unmap()
	if la == ta {
		return true
	}
	if la.IsUnspecified() {
		// :: can accept IPv4 connections when dual-stack is enabled; 0.0.0.0 accepts IPv4.
		return la.Is6() || ta.Is4()
	}
	return false
}
