package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/recovery"
)

var errTerminalUnidentified = errors.New("Mesh cannot identify this terminal, so it cannot remember sessions for it; export MESH_TERMINAL_ID to name it yourself")

var errTerminalUnbound = errors.New("this terminal has not opened a Mesh session yet; run mesh <host> to start one")

func (a *application) backCommand() *cobra.Command {
	var shell, command, agent, raw, takeover, forget bool
	var detachKey string
	cmd := &cobra.Command{
		Use:   "back",
		Short: "Return this terminal to the session it last opened",
		Long: "Return this terminal to the session it last opened.\n\n" +
			"Each terminal tab remembers the session it opened. If the host rebooted and\n" +
			"the session was interrupted, `back` recovers it in place, the way `mesh recover`\n" +
			"does, and rebinds this terminal to the replacement.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			identity, ok := a.dependencies.Terminal()
			if !ok {
				return errTerminalUnidentified
			}
			if forget {
				if err := forgetTerminalBinding(identity.Key); err != nil {
					return err
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "This terminal is no longer bound to a session")
				return err
			}
			binding, found := loadTerminalBinding(identity.Key)
			if !found {
				return errTerminalUnbound
			}
			resolved, err := a.resolveBinding(cmd.Context(), identity, binding)
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
			// recoverSession attaches a live session and relaunches an
			// interrupted one, so a single call covers both outcomes, and the
			// attach it ends in rebinds this terminal to whatever it landed on.
			return a.recoverSession(cmd, resolved, action, detachKey, raw, takeover)
		},
	}
	cmd.Flags().BoolVar(&shell, "shell", false, "open a shell in the saved directory")
	cmd.Flags().BoolVar(&command, "command", false, "explicitly restart the saved command")
	cmd.Flags().BoolVar(&agent, "agent", false, "resume the exact saved agent conversation")
	cmd.MarkFlagsMutuallyExclusive("shell", "command", "agent")
	cmd.Flags().BoolVar(&raw, "raw", false, "attach without changing terminal mode")
	cmd.Flags().BoolVar(&takeover, "takeover", false, "take over if the session is already attached")
	cmd.Flags().BoolVar(&forget, "forget", false, "drop this terminal's binding and return")
	cmd.Flags().StringVar(&detachKey, "detach-key", "ctrl+]", "key that detaches from the session")
	return cmd
}

// resolveBinding keeps host lookup failures retryable. Only a confirmed reused
// session ID invalidates the remembered binding.
func (a *application) resolveBinding(ctx context.Context, identity TerminalIdentity, binding TerminalBinding) (resolvedSession, error) {
	var resolved resolvedSession
	var err error
	if binding.HostID == "" {
		var local Session
		local, err = Find(binding.SessionID)
		resolved.local = &local
	} else {
		resolved, err = a.resolveSavedTarget(ctx, recovery.Target{HostID: binding.HostID, SessionID: binding.SessionID})
	}
	if err != nil {
		return resolvedSession{}, fmt.Errorf("cannot return to session %s; terminal binding retained: %w", binding.SessionID, err)
	}
	// Session ids are four characters and are freed with their directory, so an
	// id can be handed out again. Creation time is what separates the session
	// this terminal opened from a later one wearing its id.
	if err := bindingStillDescribes(binding, resolved); err != nil {
		_ = forgetTerminalBinding(identity.Key)
		return resolvedSession{}, err
	}
	return resolved, nil
}

func bindingStillDescribes(binding TerminalBinding, resolved resolvedSession) error {
	if binding.CreatedAt.IsZero() {
		return nil
	}
	created := resolved.remote.CreatedAt
	if resolved.local != nil {
		created = resolved.local.CreatedAt
	}
	if created.IsZero() || created.Equal(binding.CreatedAt) {
		return nil
	}
	return fmt.Errorf("session %s was created at %s, not %s, so it is a different session reusing the id; this terminal is unbound",
		binding.SessionID, created.UTC().Format("2006-01-02 15:04:05"), binding.CreatedAt.UTC().Format("2006-01-02 15:04:05"))
}
