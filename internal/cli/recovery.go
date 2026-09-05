package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/worker"
)

var errRecoveryUnsupported = errors.New("this host does not support saved recovery; update Mesh on the host")

func (a *application) recoverCommand() *cobra.Command {
	var shell, command, agent, raw, takeover bool
	var detachKey string
	cmd := &cobra.Command{
		Use: "recover SESSION", Short: "Recover a saved session and retain its previous output",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := a.resolveRecoverySession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			action := recovery.ActionDefault
			if shell {
				action = recovery.ActionShell
			}
			if command {
				action = recovery.ActionCommand
			}
			if agent {
				action = recovery.ActionAgent
			}
			return a.recoverSession(cmd, resolved, action, detachKey, raw, takeover)
		},
	}
	cmd.Flags().BoolVar(&shell, "shell", false, "open a shell in the saved directory")
	cmd.Flags().BoolVar(&command, "command", false, "explicitly restart the saved command")
	cmd.Flags().BoolVar(&agent, "agent", false, "resume the exact saved agent conversation")
	cmd.MarkFlagsMutuallyExclusive("shell", "command", "agent")
	cmd.Flags().BoolVar(&raw, "raw", false, "attach without changing terminal mode")
	cmd.Flags().BoolVar(&takeover, "takeover", false, "take over if the recovered session is already attached")
	cmd.Flags().StringVar(&detachKey, "detach-key", "ctrl+]", "key that detaches from the session")
	return cmd
}

func (a *application) resolveRecoverySession(ctx context.Context, id string) (resolvedSession, error) {
	id, err := session.ParseID(id)
	if err != nil {
		return resolvedSession{}, err
	}
	local, err := Find(id)
	if err == nil {
		return resolvedSession{local: &local}, nil
	}
	if !errors.Is(err, ErrNoLocalSession) {
		return resolvedSession{}, err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return resolvedSession{}, err
	}
	return a.resolveSession(ctx, hosts, id)
}

func localRecoveryConfig() (recovery.Config, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return recovery.Config{}, err
	}
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return recovery.Config{}, fmt.Errorf("load recovery host: %w", err)
	}
	root, err := paths.SessionsDir()
	if err != nil {
		return recovery.Config{}, err
	}
	return recovery.Config{SessionsDir: root, HostID: host.ID, BootID: worker.BootID(), Runtime: worker.RecoveryRuntime{
		SessionsDir: root, HostID: host.ID, Env: os.Environ(),
	}}, nil
}

func (a *application) recoverSession(cmd *cobra.Command, resolved resolvedSession, action recovery.Action, detachKey string, raw, takeover bool) error {
	seen := make(map[recovery.Target]bool)
	for range protocol.MaxContainingSessions + 1 {
		target, err := resolvedTargetIdentity(resolved)
		if err != nil {
			return err
		}
		key := recovery.Target{HostID: target.HostID, SessionID: target.SessionID}
		if seen[key] {
			return fmt.Errorf("saved recovery targets form a cycle at %s/%s", key.HostID, key.SessionID)
		}
		seen[key] = true
		result, err := a.requestRecovery(cmd, resolved, action)
		if err != nil {
			return err
		}
		if result.Remote != nil {
			resolved, err = a.resolveSavedTarget(cmd.Context(), *result.Remote)
			if err != nil {
				return fmt.Errorf("saved target unavailable: %w; retry or use --shell on the original session", err)
			}
			continue
		}
		if result.OriginalCwd != "" && result.OriginalCwd != result.Cwd {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Saved directory %q is unavailable; opening shell in %q.\n", result.OriginalCwd, result.Cwd)
		}
		reportAgentRecoveryStatus(cmd.ErrOrStderr(), result)
		resolved, err = recoveredSession(resolved, result)
		if err != nil {
			return err
		}
		if result.Existing && result.State == worker.StateInterrupted {
			continue
		}
		resolved.ifDetached = !takeover
		return a.attachResolved(cmd, resolved, detachKey, raw, nil)
	}
	return errors.New("saved recovery target chain is too long")
}

func (a *application) requestRecovery(cmd *cobra.Command, resolved resolvedSession, action recovery.Action) (recovery.Result, error) {
	cols, rows := terminalSize(a.dependencies.Stdout)
	request := recovery.Request{Action: action, Cols: cols, Rows: rows, Term: clientTerm(), Depth: SessionDepth() + 1}
	if resolved.local != nil {
		config, err := localRecoveryConfig()
		if err != nil {
			return recovery.Result{}, err
		}
		request.SessionID = resolved.local.ID
		return recovery.Recover(cmd.Context(), config, request)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), remoteCreateTimeout)
	defer cancel()
	conn, host, err := openVerifiedHostInfo(ctx, *resolved.host, a.dependencies.DialHost)
	if err != nil {
		return recovery.Result{}, err
	}
	defer func() { _ = conn.Close() }()
	if !host.RecoverySupported {
		return recovery.Result{}, errRecoveryUnsupported
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return recovery.Result{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type: protocol.TypeRecover, SessionID: resolved.remote.ID, RequestID: requestID,
		RecoveryAction: string(action), Cols: cols, Rows: rows, Term: request.Term, Depth: request.Depth,
	})
	if err != nil {
		return recovery.Result{}, err
	}
	return recoveredResponse(response)
}

func recoveredResponse(response protocol.Control) (recovery.Result, error) {
	if response.Type == protocol.TypeError {
		return recovery.Result{}, fmt.Errorf("recover session: %s", response.Message)
	}
	if response.Type != protocol.TypeRecovered || response.RecoveryResult == nil {
		return recovery.Result{}, errors.New("host returned an invalid recovery response")
	}
	result := *response.RecoveryResult
	if result.Remote != nil {
		err := protocol.ValidateSessionIdentity(protocol.SessionIdentity{HostID: result.Remote.HostID, SessionID: result.Remote.SessionID})
		return result, err
	}
	_, err := session.ParseID(result.SessionID)
	return result, err
}

func recoveredSession(source resolvedSession, result recovery.Result) (resolvedSession, error) {
	if source.local != nil {
		current, err := Find(result.SessionID)
		return resolvedSession{local: &current}, err
	}
	state := result.State
	if state == "" {
		state = worker.StateRunning
	}
	return resolvedSession{host: source.host, remote: protocol.SessionInfo{
		ID: result.SessionID, HostID: source.host.ID, State: state,
	}}, nil
}

func (a *application) resolveSavedTarget(ctx context.Context, target recovery.Target) (resolvedSession, error) {
	config, err := localRecoveryConfig()
	if err != nil {
		return resolvedSession{}, err
	}
	if target.HostID == config.HostID {
		current, err := Find(target.SessionID)
		return resolvedSession{local: &current}, err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return resolvedSession{}, err
	}
	for _, host := range hosts {
		if host.ID != target.HostID {
			continue
		}
		rows, err := a.queryHost(ctx, host)
		if err != nil {
			return resolvedSession{}, err
		}
		return exactRecoveryTarget(host, rows, target.SessionID)
	}
	return resolvedSession{}, fmt.Errorf("host %s is not in this machine's address book", target.HostID)
}

func exactRecoveryTarget(host HostRecord, rows []protocol.SessionInfo, id string) (resolvedSession, error) {
	for _, row := range rows {
		if row.ID == id {
			return resolvedSession{host: &host, remote: row}, nil
		}
	}
	return resolvedSession{}, fmt.Errorf("session %s is missing on %s", id, host.Alias)
}

func (a *application) recoveryCommandCommand() *cobra.Command {
	var clear bool
	var directory string
	cmd := &cobra.Command{
		Use: "recovery-command SESSION -- PROGRAM ARG...", Short: "Save an explicit restart command",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clear && len(args) != 1 || !clear && (len(args) < 2 || cmd.ArgsLenAtDash() != 1) {
				return errors.New("use recovery-command SESSION -- PROGRAM ARG... or recovery-command SESSION --clear")
			}
			resolved, err := a.resolveRecoverySession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var recipe *recovery.Command
			if !clear {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				if resolved.host != nil {
					cwd = resolved.remote.Cwd
					if resolved.remote.Recovery != nil {
						cwd = resolved.remote.Recovery.ShellDirectory
					}
				}
				if directory != "" {
					cwd = directory
				}
				recipe = &recovery.Command{Argv: args[1:], Cwd: filepath.Clean(cwd)}
			}
			return a.configureRecoveryCommand(cmd.Context(), resolved, recipe)
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "clear the explicit restart command")
	cmd.Flags().StringVar(&directory, "cwd", "", "absolute directory on the session host for the command")
	return cmd
}

func (a *application) configureRecoveryCommand(ctx context.Context, resolved resolvedSession, recipe *recovery.Command) error {
	if resolved.local != nil {
		config, err := localRecoveryConfig()
		if err != nil {
			return err
		}
		return recovery.ConfigureCommand(ctx, config, resolved.local.ID, recipe)
	}
	conn, err := openVerifiedHost(ctx, *resolved.host, a.dependencies.DialControl)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type: protocol.TypeRecoveryCommand, RequestID: requestID, SessionID: resolved.remote.ID,
		RecoveryCommand: recipe, ClearRecoveryCommand: recipe == nil,
	})
	if err != nil {
		return err
	}
	if response.Type != protocol.TypeOK {
		return fmt.Errorf("configure recovery command: %s", response.Message)
	}
	return nil
}
