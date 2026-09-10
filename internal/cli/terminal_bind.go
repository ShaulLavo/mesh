package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// bindTerminal records the session this terminal is opening, before the attach
// begins. It cannot be done afterwards: a host crash never returns from the
// read, a closed tab runs no deferred code, and Attach is handed a background
// context, so the end of an attachment is not a place code reliably reaches.
//
// It reports a function that puts the previous binding back, for the claim
// failures that mean this attachment never started.
func (a *application) bindTerminal(binding TerminalBinding) func() {
	unchanged := func() {}
	identity, ok := a.dependencies.Terminal()
	if !ok {
		return unchanged
	}
	previous, had := loadTerminalBinding(identity.Key)
	binding.Source = identity.Source
	binding.BoundAt = a.dependencies.Now().UTC()
	// A terminal that forgets its session is a smaller problem than an attach
	// that refuses to start, so this stays best-effort.
	if err := saveTerminalBinding(identity.Key, binding); err != nil {
		return unchanged
	}
	return func() {
		if had {
			_ = saveTerminalBinding(identity.Key, previous)
			return
		}
		_ = forgetTerminalBinding(identity.Key)
	}
}

// bindingFor describes a resolved session well enough to find it again. It
// deliberately stores no cwd or command: a picker row can be arbitrarily stale
// and a freshly created session carries neither, so recording them would put
// yesterday's values, or blanks, where the truth is expected.
func bindingFor(resolved resolvedSession) TerminalBinding {
	if resolved.local != nil {
		return TerminalBinding{
			SessionID: resolved.local.ID,
			OriginID:  resolved.local.RecoveredFrom,
			CreatedAt: resolved.local.CreatedAt,
		}
	}
	binding := TerminalBinding{
		SessionID: resolved.remote.ID,
		OriginID:  resolved.remote.RecoveredFrom,
		CreatedAt: resolved.remote.CreatedAt,
	}
	if resolved.host != nil {
		binding.HostID = resolved.host.ID
	}
	// A stale row's timestamps came out of the cache, and a wrong creation time
	// would later read as "a different session reusing the id".
	if resolved.stale {
		binding.CreatedAt = time.Time{}
	}
	return binding
}

// claimFailed reports the errors that mean the attachment never took hold, so
// the terminal keeps pointing at whatever it was on before.
func claimFailed(err error) bool {
	return errors.Is(err, ErrSessionAttached) ||
		errors.Is(err, ErrSessionUnavailable) ||
		errors.Is(err, ErrAttachDetachedUnsupported)
}

// noteTerminalBinding points a returning tab at its own session. The picker
// still opens afterwards: bare `mesh` has always been total and side-effect
// free, and a tab whose host just crashed should not have something attached to
// it without being asked.
func (a *application) noteTerminalBinding(cmd *cobra.Command, hosts []HostRecord) {
	identity, ok := a.dependencies.Terminal()
	if !ok {
		return
	}
	binding, found := loadTerminalBinding(identity.Key)
	if !found {
		return
	}
	where := binding.SessionID
	for _, host := range hosts {
		if binding.HostID != "" && host.ID == binding.HostID {
			where = binding.SessionID + " on " + host.Alias
			break
		}
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "This terminal last opened %s — run `mesh back` to return to it.\n", where)
}
