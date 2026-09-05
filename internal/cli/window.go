package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

func (a *application) runWindow(cmd *cobra.Command, take bool, detachKey string, raw bool) error {
	if !term.IsTerminal(a.dependencies.Stdin.Fd()) || !term.IsTerminal(a.dependencies.Stdout.Fd()) {
		return errors.New("--window needs a terminal")
	}
	if _, err := a.attachmentOptions(cmd, detachKey, raw); err != nil {
		return err
	}
	if len(a.dependencies.Containment(cmd.Context())) != 0 {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Already inside a Mesh session; starting a plain shell."); err != nil {
			return err
		}
		shell := exec.CommandContext(cmd.Context(), defaultShell()) //nolint:gosec // the user's SHELL is the requested terminal command
		shell.Stdin, shell.Stdout, shell.Stderr = a.dependencies.Stdin, a.dependencies.Stdout, a.dependencies.Stderr
		if err := shell.Run(); err != nil {
			var exited *exec.ExitError
			if errors.As(err, &exited) {
				return statusError{code: exited.ExitCode()}
			}
			return fmt.Errorf("start shell: %w", err)
		}
		return nil
	}
	for {
		local, err := localPickerCatalog()
		if err != nil {
			return err
		}
		rows := windowSessionRows(local.Sessions)
		if take {
			for _, row := range rows {
				if row.State != worker.StateDetached {
					continue
				}
				err := a.attachWindowSession(cmd, row.ID, detachKey, raw)
				if errors.Is(err, ErrSessionAttached) || errors.Is(err, ErrAttachDetachedUnsupported) || errors.Is(err, ErrSessionUnavailable) {
					continue
				}
				return err
			}
			return a.startWindowSession(cmd, detachKey, raw)
		}
		if len(rows) == 0 || (rows[0].State != worker.StateDetached && rows[0].State != worker.StateInterrupted) {
			return a.startWindowSession(cmd, detachKey, raw)
		}
		if a.dependencies.WindowPicker == nil {
			return errors.New("the window prompt is unavailable; use --window --take")
		}
		hosts, err := LoadHosts()
		if err != nil {
			return err
		}
		aliases := map[string]string{local.Host.ID: localHostAlias}
		for _, host := range hosts {
			aliases[host.ID] = host.Alias
		}
		selection, err := a.dependencies.WindowPicker(cmd.Context(), WindowInput{
			Sessions: rows, HostAlias: localHostAlias, HostID: local.Host.ID, HostAliases: aliases,
			Inspect: inspectLocalSession,
			Action: func(ctx context.Context, request PickerSessionActionRequest) error {
				return a.localPickerSessionAction(ctx, request)
			},
		})
		if err != nil {
			return err
		}
		switch {
		case selection.FullPicker:
			return a.runPickerOpen(cmd, hosts, detachKey, raw, localHostAlias)
		case selection.New:
			return a.startWindowSession(cmd, detachKey, raw)
		case selection.SessionID != "":
			current, err := Find(selection.SessionID)
			if err != nil {
				return err
			}
			if selection.Relaunch {
				return a.relaunchSession(cmd, resolvedSession{local: &current}, detachKey, raw, true)
			}
			err = a.attachWindowSession(cmd, current.ID, detachKey, raw)
			if errors.Is(err, ErrSessionAttached) || errors.Is(err, ErrSessionUnavailable) {
				continue
			}
			return err
		default:
			return nil
		}
	}
}

func (a *application) attachPickerSession(cmd *cobra.Command, resolved resolvedSession, takeOver bool, detachKey string, raw bool, containing []protocol.SessionIdentity) error {
	resolved.ifDetached = !takeOver
	return a.attachResolvedWithContainment(cmd, resolved, detachKey, raw, nil, containing)
}

// Relaunch retains the old record until creation has published an answering
// worker. Both entry points use the recorded launch directory, not the client's.
func (a *application) relaunchSession(cmd *cobra.Command, resolved resolvedSession, detachKey string, raw bool, window bool) error {
	if resolved.local != nil {
		old, err := Find(resolved.local.ID)
		if err != nil {
			return err
		}
		if old.State() != worker.StateInterrupted {
			return fmt.Errorf("session %s is %s, not interrupted", old.ID, old.State())
		}
		current, socket, err := a.createLocalSession(cmd, old.Command, old.Cwd, false)
		if err != nil {
			return err
		}
		if err := forgetLocalSession(cmd.Context(), old); err != nil {
			return fmt.Errorf("new session %s is running; forget old session %s: %w", current.ID, old.ID, err)
		}
		initial := uint64(0)
		if window {
			err := a.attachWindow(cmd, current, socket, &initial, true, detachKey, raw)
			if errors.Is(err, ErrSessionAttached) || errors.Is(err, ErrSessionUnavailable) {
				return a.startWindowSession(cmd, detachKey, raw)
			}
			return err
		}
		return a.attachResolved(cmd, resolvedSession{local: &current, ifDetached: true}, detachKey, raw, &initial)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), remoteCreateTimeout)
	defer cancel()
	rows, err := a.queryHost(ctx, *resolved.host)
	if err != nil {
		return err
	}
	var old *protocol.SessionInfo
	for _, row := range rows {
		if row.ID == resolved.remote.ID {
			copy := row
			old = &copy
			break
		}
	}
	if old == nil || old.State != worker.StateInterrupted {
		return fmt.Errorf("session %s on %s is no longer interrupted", resolved.remote.ID, resolved.host.Alias)
	}
	cols, height := terminalSize(a.dependencies.Stdout)
	id, err := createRemoteSessionInDirectory(ctx, *resolved.host, a.dependencies.DialHost, old.Command, old.Cwd, cols, height)
	if err != nil {
		return err
	}
	if err := controlRemoteSession(ctx, *resolved.host, a.dependencies.DialControl, old.ID, protocol.TypeRemove, ""); err != nil {
		return fmt.Errorf("new session %s is running; forget old session %s: %w", id, old.ID, err)
	}
	initial := uint64(0)
	return a.attachResolved(cmd, resolvedSession{host: resolved.host, ifDetached: true, remote: protocol.SessionInfo{
		ID: id, HostID: resolved.host.ID, State: worker.StateRunning,
	}}, detachKey, raw, &initial)
}

func windowSessionRows(source []protocol.SessionInfo) []protocol.SessionInfo {
	rows := make([]protocol.SessionInfo, 0, len(source))
	for _, row := range source {
		if row.State == worker.StateDetached || row.State == worker.StateInterrupted || row.State == worker.StateRunning {
			rows = append(rows, row)
		}
	}
	rank := func(state string) int {
		switch state {
		case worker.StateDetached:
			return 0
		case worker.StateInterrupted:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rank(rows[i].State) != rank(rows[j].State) {
			return rank(rows[i].State) < rank(rows[j].State)
		}
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	return rows
}

func (a *application) startWindowSession(cmd *cobra.Command, detachKey string, raw bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("read window working directory: %w", err)
	}
	for {
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		current, socket, err := a.createLocalSession(cmd, []string{defaultShell()}, cwd, false)
		if err != nil {
			return err
		}
		initial := uint64(0)
		err = a.attachWindow(cmd, current, socket, &initial, true, detachKey, raw)
		if errors.Is(err, ErrSessionAttached) || errors.Is(err, ErrSessionUnavailable) {
			continue
		}
		return err
	}
}

func (a *application) attachWindowSession(cmd *cobra.Command, id, detachKey string, raw bool) error {
	current, err := Find(id)
	if err != nil {
		return err
	}
	if current.State() == worker.StateRunning {
		return ErrSessionAttached
	}
	if current.State() != worker.StateDetached {
		return fmt.Errorf("session %s is %s", id, current.State())
	}
	return a.attachWindow(cmd, current, paths.Socket(current.Dir), nil, true, detachKey, raw)
}

func (a *application) attachWindow(cmd *cobra.Command, current Session, socket string, lastSeq *uint64, ifDetached bool, detachKey string, raw bool) error {
	opts, err := a.attachmentOptions(cmd, detachKey, raw)
	if err != nil {
		return err
	}
	opts.SocketPath, opts.SessionID, opts.LastSeq, opts.IfDetached = socket, current.ID, lastSeq, ifDetached
	opts.ContainingSessions = a.dependencies.Containment(cmd.Context())
	if len(opts.ContainingSessions) > 0 {
		target, err := resolvedTargetIdentity(resolvedSession{local: &current})
		if err != nil {
			return err
		}
		opts.HostID = target.HostID
	}
	result, err := Attach(opts)
	if err != nil {
		return err
	}
	if result.Exited && result.ExitCode != 0 {
		return statusError{code: result.ExitCode}
	}
	return nil
}

func (a *application) attachmentOptions(cmd *cobra.Command, detachKey string, raw bool) (AttachOptions, error) {
	key, parsedRaw, err := ParseDetachKey(detachKey)
	if err != nil {
		return AttachOptions{}, err
	}
	leave := "ctrl+^"
	if flag := cmd.Flag("leave-key"); flag != nil {
		leave = flag.Value.String()
	}
	leaveKey, disableLeave, err := ParseDetachKey(leave)
	if err != nil {
		return AttachOptions{}, fmt.Errorf("--leave-key: %w", err)
	}
	dynamicKey := key
	if detachKey == "" {
		dynamicKey = DefaultDetachKey
	}
	if !raw && !parsedRaw && !disableLeave && leaveKey == dynamicKey {
		return AttachOptions{}, errors.New("--leave-key and --detach-key must differ")
	}
	return AttachOptions{
		DetachKey: key, DetachKeyExplicit: detachKey != "", Raw: raw || parsedRaw,
		LeaveKey: leaveKey, DisableLeaveKey: disableLeave,
		In: a.dependencies.Stdin, Out: a.dependencies.Stdout,
		Stderr: a.dependencies.Stderr,
	}, nil
}

func (a *application) createLocalSession(cmd *cobra.Command, command []string, cwd string, requireDaemon bool) (Session, string, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return Session{}, "", err
	}
	socket := meshdaemon.SocketPath(stateDir)
	cols, rows := terminalSize(a.dependencies.Stdout)
	ctx, cancel := context.WithTimeout(cmd.Context(), remoteCreateTimeout)
	id, err := CreateViaDaemon(ctx, DaemonCreateOptions{
		SocketPath: socket, Command: command, Cwd: cwd, Cols: cols, Rows: rows,
	})
	cancel()
	if errors.Is(err, ErrDaemonUnavailable) && !requireDaemon {
		current, spawnErr := Spawn(command, cwd)
		return current, paths.Socket(current.Dir), spawnErr
	}
	if err != nil {
		return Session{}, "", err
	}
	dir, err := paths.SessionDir(id)
	if err != nil {
		return Session{}, "", err
	}
	return Session{Meta: worker.Meta{ID: id, Command: command, Cwd: cwd}, Dir: dir, Alive: true}, socket, nil
}

func localPickerCatalog() (HostSessions, error) {
	rows, err := localSessionRows()
	if err != nil {
		return HostSessions{}, err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return HostSessions{}, err
	}
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return HostSessions{}, fmt.Errorf("load local identity: %w", err)
	}
	for i := range rows {
		rows[i].HostID = host.ID
	}
	return HostSessions{Local: true, Host: HostRecord{Alias: localHostAlias, ID: host.ID}, Sessions: rows}, nil
}

func inspectLocalSession(parent context.Context, request PickerInspectRequest) (SessionInspection, error) {
	if err := protocol.ValidateInspectDimensions(request.PreviewCols, request.PreviewRows); err != nil {
		return SessionInspection{}, err
	}
	id, err := protocol.NewSessionID(request.SessionID)
	if err != nil {
		return SessionInspection{}, err
	}
	dir, err := paths.SessionDir(id.String())
	if err != nil {
		return SessionInspection{}, err
	}
	ctx, cancel := context.WithTimeout(parent, localQueryTimeout)
	defer cancel()
	requestID, err := newDaemonRequestID()
	if err != nil {
		return SessionInspection{}, err
	}
	response, err := daemonControlRequest(ctx, paths.Socket(dir), protocol.Control{
		Type: protocol.TypeInspect, RequestID: requestID, SessionID: request.SessionID,
		PreviewCols: request.PreviewCols, PreviewRows: request.PreviewRows,
	})
	if err != nil {
		return SessionInspection{}, err
	}
	return validateInspectionResponse(HostRecord{Alias: localHostAlias}, request.SessionID, request.PreviewCols, request.PreviewRows, response)
}

func (a *application) localPickerSessionAction(ctx context.Context, request PickerSessionActionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := Find(request.SessionID)
	if err != nil {
		return err
	}
	switch request.Action {
	case PickerKillSession:
		return Kill(current)
	case PickerRemoveSession:
		if current.Alive {
			return fmt.Errorf("session %s is still running; kill it before removing it", current.ID)
		}
		return forgetLocalSession(ctx, current)
	default:
		return fmt.Errorf("unknown picker session action %d", request.Action)
	}
}

func forgetLocalSession(parent context.Context, current Session) error {
	if alive(current.Dir) {
		return fmt.Errorf("session %s is still running; kill it before removing it", current.ID)
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, remoteConnectTimeout)
	defer cancel()
	response, err := daemonControlRequest(ctx, meshdaemon.SocketPath(stateDir), protocol.Control{
		Type: protocol.TypeRemove, SessionID: current.ID, RequestID: requestID,
	})
	if err == nil {
		if response.Type == protocol.TypeOK && response.SessionID == current.ID {
			return nil
		}
		return daemonResponseError("forget "+current.ID, response.Message)
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		return err
	}
	// The daemon may have a durable row from before it stopped. Keep a marker
	// until reconciliation can retire the row and directory together.
	marker, err := os.OpenFile(paths.Forgotten(current.Dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("forget session %s: %w", current.ID, err)
	}
	if err := marker.Sync(); err != nil {
		_ = marker.Close()
		return fmt.Errorf("sync forgotten session %s: %w", current.ID, err)
	}
	return marker.Close()
}
