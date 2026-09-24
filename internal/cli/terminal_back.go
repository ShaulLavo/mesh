package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/recovery"
)

var errTerminalUnidentified = errors.New("Mesh cannot identify this terminal, so it cannot remember sessions for it; export MESH_TERMINAL_ID to name it yourself")

var errTerminalInsideSession = errors.New("this is a Mesh session, not a terminal of its own; `mesh back` belongs in the terminal you started the session from")

var errTerminalUnbound = errors.New("this terminal has not opened a Mesh session yet; run mesh <host> to start one")

func (a *application) backCommand() *cobra.Command {
	var shell, command, agent, raw, takeover, forget bool
	var detachKey string
	cmd := &cobra.Command{
		Use:   "back",
		Short: "Return this terminal to the session it last opened",
		Long: "Return this terminal to the session it last opened.\n\n" +
			"Each terminal remembers the session it opened. If the host rebooted and the\n" +
			"session was interrupted, `back` recovers it the way `mesh recover` does and\n" +
			"rebinds this terminal to the replacement. Recovering a session that was running\n" +
			"an agent resumes that conversation; use --shell for a plain shell instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			identity, ok := a.dependencies.Terminal()
			if !ok {
				// Telling someone to export MESH_TERMINAL_ID is useless advice
				// when the refusal happened before any variable was read.
				if InsideMeshSession() {
					return errTerminalInsideSession
				}
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
			// interrupted one, so one call covers both outcomes, and the attach
			// it ends in rebinds this terminal to whatever it landed on.
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

// resolveBinding turns a remembered session into something attachable.
//
// It keeps the binding when it cannot. A host that is down answers no query, so
// an outage is indistinguishable here from a session that no longer exists, and
// discarding the record on a failed lookup would erase the terminal's memory
// during exactly the crash this command exists for. Only a session that
// resolves and proves to be a different one is forgotten.
func (a *application) resolveBinding(ctx context.Context, identity TerminalIdentity, binding TerminalBinding) (resolvedSession, error) {
	var resolved resolvedSession
	var err error
	// The remembered host decides where to look. Searching by id alone would
	// resolve a local session that happens to share the id, and could report an
	// ambiguity this record already answers.
	if binding.HostID == "" {
		var local Session
		local, err = Find(binding.SessionID)
		resolved.local = &local
	} else {
		resolved, err = a.resolveSavedTarget(ctx, recovery.Target{HostID: binding.HostID, SessionID: binding.SessionID})
	}
	if err != nil {
		return resolvedSession{}, fmt.Errorf("cannot return to %s; terminal binding retained: %w", describeBoundSession(binding), err)
	}
	// Session ids are four characters and are freed with their directory, so an
	// id can be handed out again. Creation time is what separates the session
	// this terminal opened from a later one wearing its id.
	if err := bindingStillDescribes(binding, resolved); err != nil {
		_ = forgetTerminalBinding(identity.Key)
		return resolvedSession{}, err
	}
	rememberCreation(identity, binding, resolved)
	return resolved, nil
}

// rememberCreation fills in a creation time the binding could not know when it
// was written. A session is bound the moment it is created, before the host has
// reported anything about it, so the guard against a recycled id is blank until
// the first lookup that can supply one.
func rememberCreation(identity TerminalIdentity, binding TerminalBinding, resolved resolvedSession) {
	if !binding.CreatedAt.IsZero() {
		return
	}
	created := resolved.remote.CreatedAt
	if resolved.local != nil {
		created = resolved.local.CreatedAt
	}
	if created.IsZero() || resolved.stale {
		return
	}
	binding.CreatedAt = created
	_ = saveTerminalBinding(identity.Key, binding)
}

func describeBoundSession(binding TerminalBinding) string {
	if binding.HostID == "" {
		return "session " + binding.SessionID
	}
	hosts, err := LoadHosts()
	if err == nil {
		for _, host := range hosts {
			if host.ID == binding.HostID {
				return "session " + binding.SessionID + " on " + host.Alias
			}
		}
	}
	return "session " + binding.SessionID
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
