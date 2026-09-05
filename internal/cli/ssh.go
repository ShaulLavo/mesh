package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/worker"
)

type TerminalPickerFactory func(*os.File, io.Writer, string, func() terminal.Size, <-chan terminal.Size) PickerFunc

type sshApplication struct {
	socket string
	picker TerminalPickerFactory
}

type sshAttachment struct {
	id         string
	ifDetached bool
	lastSeq    *uint64
}

// NewSSHSessionHandler serves only this daemon's sessions. Selecting another
// host here would make the SSH host a terminal proxy and break direct routing.
func NewSSHSessionHandler(stateDir string, picker TerminalPickerFactory) sshd.SessionHandler {
	app := sshApplication{socket: daemon.SocketPath(stateDir), picker: picker}
	return app.run
}

func (a sshApplication) run(ctx context.Context, client sshd.Session) (int, error) {
	if client.Command.Kind == sshd.CommandList {
		catalog, err := a.catalog(ctx)
		if err != nil {
			return 1, err
		}
		return 0, writeProtocolSessions(client.Out, time.Now(), []HostSessions{catalog})
	}
	if a.picker == nil {
		return 1, errors.New("SSH session picker is unavailable")
	}
	var target *sshAttachment
	if client.Command.Kind == sshd.CommandAttach {
		target = &sshAttachment{id: client.Command.SessionID}
	}
	if client.Command.Kind == sshd.CommandRecover {
		var err error
		target, err = a.relaunch(ctx, client, client.Command.SessionID, client.Command.RecoveryAction)
		if err != nil {
			return 1, err
		}
	}
	picker := a.picker(client.In, client.Out, client.Terminal, client.Size, client.WindowChanges)
	for ctx.Err() == nil {
		selected, err := a.nextAttachment(ctx, client, picker, target)
		if err != nil || selected == nil {
			return sshResult(err)
		}
		result, err := a.attach(ctx, client, *selected)
		if err != nil {
			return 1, err
		}
		if result.Exited {
			return result.ExitCode, nil
		}
		if !result.Detached {
			return 0, nil
		}
		target = nil
	}
	return sshResult(ctx.Err())
}

func sshResult(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func (a sshApplication) nextAttachment(ctx context.Context, client sshd.Session, picker PickerFunc, target *sshAttachment) (*sshAttachment, error) {
	if target != nil {
		return target, nil
	}
	selected, err := a.pick(ctx, picker)
	if err != nil {
		return nil, err
	}
	if err := validatePickerSelection(selected); err != nil {
		return nil, err
	}
	if selected.HostAlias == "" && selected.SessionID == "" {
		return nil, nil
	}
	if selected.HostAlias != localHostAlias || selected.Wake {
		return nil, errors.New("SSH can select only sessions on this host")
	}
	if selected.New {
		cwd, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find shell home directory: %w", err)
		}
		return a.create(ctx, client, []string{defaultShell()}, cwd)
	}
	if selected.Relaunch {
		return a.relaunch(ctx, client, selected.SessionID, selected.RecoveryAction)
	}
	if selected.SessionID != "" {
		id, err := session.ParseID(selected.SessionID)
		return &sshAttachment{id: id, ifDetached: !selected.TakeOver}, err
	}
	catalog, err := a.catalog(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range windowSessionRows(catalog.Sessions) {
		if row.State == worker.StateDetached {
			return &sshAttachment{id: row.ID, ifDetached: true}, nil
		}
	}
	return nil, errors.New("this host has no detached sessions; select a session to take over")
}

func (a sshApplication) pick(ctx context.Context, picker PickerFunc) (PickerSelection, error) {
	catalog, err := a.catalog(ctx)
	if err != nil {
		return PickerSelection{}, err
	}
	pickerContext, cancel := context.WithCancel(ctx)
	gate := newPickerOperationGate()
	defer func() {
		cancel()
		gate.stopAndWait()
	}()
	return picker(pickerContext, PickerInput{
		Hosts: []HostSessions{catalog}, OpenHostAlias: localHostAlias,
		Refresh: func(ctx context.Context, alias string) (PickerHostSnapshot, error) {
			if !gate.begin(ctx) {
				return PickerHostSnapshot{}, context.Canceled
			}
			defer gate.done()
			if alias != localHostAlias {
				return PickerHostSnapshot{}, errors.New("SSH can refresh only this host")
			}
			current, err := a.catalog(ctx)
			return PickerHostSnapshot{Sessions: current}, err
		},
		Inspect: func(ctx context.Context, request PickerInspectRequest) (SessionInspection, error) {
			if !gate.begin(ctx) {
				return SessionInspection{}, context.Canceled
			}
			defer gate.done()
			return a.inspect(ctx, request)
		},
		Action: func(ctx context.Context, request PickerSessionActionRequest) error {
			if !gate.begin(ctx) {
				return context.Canceled
			}
			defer gate.done()
			return a.action(ctx, request)
		},
	})
}

func (a sshApplication) catalog(parent context.Context) (HostSessions, error) {
	ctx, cancel := context.WithTimeout(parent, defaultCatalogTimeout)
	defer cancel()
	rows, err := ListViaDaemon(ctx, a.socket)
	if err != nil {
		return HostSessions{}, err
	}
	host := HostRecord{Alias: localHostAlias}
	if len(rows) > 0 {
		host.ID = rows[0].HostID
	}
	return HostSessions{Local: true, Host: host, Sessions: rows}, nil
}

func (a sshApplication) request(parent context.Context, timeout time.Duration, request protocol.Control) (protocol.Control, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	id, err := newDaemonRequestID()
	if err != nil {
		return protocol.Control{}, err
	}
	request.RequestID = id
	return daemonControlRequest(ctx, a.socket, request)
}

func (a sshApplication) inspect(ctx context.Context, request PickerInspectRequest) (SessionInspection, error) {
	if request.HostAlias != localHostAlias {
		return SessionInspection{}, errors.New("SSH can inspect only this host")
	}
	if err := protocol.ValidateInspectDimensions(request.PreviewCols, request.PreviewRows); err != nil {
		return SessionInspection{}, err
	}
	response, err := a.request(ctx, localQueryTimeout, protocol.Control{
		Type: protocol.TypeInspect, SessionID: request.SessionID,
		PreviewCols: request.PreviewCols, PreviewRows: request.PreviewRows,
	})
	if err != nil {
		return SessionInspection{}, err
	}
	return validateInspectionResponse(HostRecord{Alias: localHostAlias}, request.SessionID, request.PreviewCols, request.PreviewRows, response)
}

func (a sshApplication) action(ctx context.Context, request PickerSessionActionRequest) error {
	if request.HostAlias != localHostAlias {
		return errors.New("SSH can change only sessions on this host")
	}
	switch request.Action {
	case PickerKillSession:
		return a.control(ctx, request.SessionID, protocol.TypeKill)
	case PickerRemoveSession:
		return a.control(ctx, request.SessionID, protocol.TypeRemove)
	default:
		return fmt.Errorf("unknown picker session action %d", request.Action)
	}
}

func (a sshApplication) control(ctx context.Context, id, operation string) error {
	response, err := a.request(ctx, 12*time.Second, protocol.Control{Type: operation, SessionID: id})
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		return daemonResponseError(operation+" "+id, response.Message)
	}
	if response.Type != protocol.TypeOK || response.SessionID != id {
		return fmt.Errorf("daemon returned an unexpected %s response", operation)
	}
	return nil
}

func (a sshApplication) create(ctx context.Context, client sshd.Session, command []string, cwd string) (*sshAttachment, error) {
	size := client.Size()
	response, err := a.request(ctx, remoteCreateTimeout, protocol.Control{
		Type: protocol.TypeCreate, Command: command, Cwd: cwd,
		Cols: size.Cols, Rows: size.Rows, Term: client.Terminal, Depth: 1,
	})
	if err != nil {
		return nil, err
	}
	id, err := validateDaemonCreateResponse(response)
	if err != nil {
		return nil, err
	}
	initial := uint64(0)
	return &sshAttachment{id: id, ifDetached: true, lastSeq: &initial}, nil
}

func (a sshApplication) relaunch(ctx context.Context, client sshd.Session, id string, action recovery.Action) (*sshAttachment, error) {
	for range protocol.MaxContainingSessions + 1 {
		size := client.Size()
		response, err := a.request(ctx, remoteCreateTimeout, protocol.Control{
			Type: protocol.TypeRecover, SessionID: id, RecoveryAction: string(action),
			Cols: size.Cols, Rows: size.Rows, Term: client.Terminal, Depth: 1,
		})
		if err != nil {
			return nil, err
		}
		result, err := recoveredResponse(response)
		if err != nil {
			return nil, err
		}
		if result.Existing && result.State == worker.StateInterrupted {
			id = result.SessionID
			continue
		}
		if result.Remote != nil {
			return nil, fmt.Errorf("saved target %s/%s is on another host; connect directly to that host or select Open shell", result.Remote.HostID, result.Remote.SessionID)
		}
		if result.OriginalCwd != "" && result.OriginalCwd != result.Cwd {
			_, _ = fmt.Fprintf(client.Err, "Saved directory %q is unavailable; opening shell in %q.\n", result.OriginalCwd, result.Cwd)
		}
		reportAgentRecoveryStatus(client.Err, result)
		return &sshAttachment{id: result.SessionID, ifDetached: true}, nil
	}
	return nil, errors.New("saved recovery attempt chain is too long")
}

func (a sshApplication) attach(ctx context.Context, client sshd.Session, target sshAttachment) (AttachResult, error) {
	input, err := uv.NewCancelReader(client.In)
	if err != nil {
		return AttachResult{}, fmt.Errorf("make SSH input cancelable: %w", err)
	}
	defer input.Close() //nolint:errcheck // releases cancellation descriptors after the relay has stopped
	return AttachWithTerminal(ctx, AttachOptions{
		SocketPath: a.socket, SessionID: target.id,
		IfDetached: target.ifDetached, LastSeq: target.lastSeq, Stderr: client.Err,
	}, AttachTerminal{
		Input: input, Output: client.Out, Size: client.Size(), Resizes: client.WindowChanges,
		CancelInput: func() { input.Cancel() },
	})
}
