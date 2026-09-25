package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

// ErrSessionHibernating means an attachment reached a worker that is stopping
// its agent. The session resumes through recovery once the worker is gone.
var ErrSessionHibernating = errors.New("session is hibernating")

// hibernationSettle bounds the wait for a hibernating worker to finish. The
// worker's kill escalates after a five-second hangup grace.
const hibernationSettle = 8 * time.Second

// Hibernation returns the marker of a session whose agent was stopped to free
// memory and whose conversation has not been resumed yet. A replacement means
// the conversation already woke elsewhere, so the row is an ordinary exit.
func Hibernation(row protocol.SessionInfo) *recovery.Hibernation {
	if row.Hibernated == nil || row.State != worker.StateExited || row.ReplacementID != "" {
		return nil
	}
	return row.Hibernated
}

// displayState is the STATE cell. A hibernated session stays exited on the
// wire so older clients keep listing its host.
func displayState(row protocol.SessionInfo) string {
	if Hibernation(row) != nil {
		return "hibernated"
	}
	return row.State
}

// localHibernation reads the marker a worker left before it stopped.
func localHibernation(current Session, replacementID string) *recovery.Hibernation {
	if current.State() != worker.StateExited || replacementID != "" {
		return nil
	}
	marker, err := recovery.ReadHibernation(current.Dir)
	if err != nil {
		return nil
	}
	return &marker
}

// hibernatedLocalSession is Hibernation for a session read straight from disk.
func hibernatedLocalSession(current Session) *recovery.Hibernation {
	if current.State() != worker.StateExited {
		return nil
	}
	host, err := identity.Load(filepath.Dir(filepath.Dir(current.Dir)))
	if err != nil {
		return nil
	}
	replacement, err := recovery.ReplacementID(current.Dir, host.ID, current.ID)
	if err != nil {
		return nil
	}
	return localHibernation(current, replacement)
}

func resolvedInterrupted(resolved resolvedSession) bool {
	if resolved.local != nil {
		return resolved.local.State() == worker.StateInterrupted
	}
	return resolved.remote.State == worker.StateInterrupted
}

func resolvedLabel(resolved resolvedSession) string {
	if resolved.local != nil {
		return resolved.local.ID
	}
	return resolved.remote.ID + " on " + resolved.host.Alias
}

func resolvedHibernation(resolved resolvedSession) *recovery.Hibernation {
	if resolved.local != nil {
		return hibernatedLocalSession(*resolved.local)
	}
	return Hibernation(resolved.remote)
}

func (a *application) hibernateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "hibernate session...",
		Short: "Stop an idle agent now; its conversation resumes on the next attach",
		Long: "Hibernate stops a detached Claude or Codex session to free its memory while\n" +
			"its conversation stays resumable. Attaching to the session later, with\n" +
			"mesh ID or from the picker, resumes the same conversation in a fresh\n" +
			"session. Plain shells cannot hibernate; use mesh kill for those.",
		Args: minimumArgs(1, "at least one session id", "mesh hibernate 7K3D  (mesh ls lists your sessions)"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := LoadHosts()
			if err != nil {
				return err
			}
			// Like rm: one refusal must not strand the rest, and every
			// outcome is reported.
			var failures []error
			for _, id := range args {
				if err := a.hibernateByID(cmd, hosts, id); err != nil {
					failures = append(failures, err)
					continue
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "hibernated %s\n", strings.ToUpper(id)); err != nil {
					return err
				}
			}
			return errors.Join(failures...)
		},
	}
}

func (a *application) hibernateByID(cmd *cobra.Command, hosts []HostRecord, id string) error {
	resolved, err := a.resolveSession(cmd.Context(), hosts, id)
	if err != nil {
		return err
	}
	if resolved.local != nil {
		switch {
		case hibernatedLocalSession(*resolved.local) != nil:
			return fmt.Errorf("session %s is already hibernated", resolved.local.ID)
		case resolved.local.Meta.State == worker.StateExited:
			return fmt.Errorf("session %s has already exited", resolved.local.ID)
		}
		err := hibernateLocal(*resolved.local, 0)
		if err != nil && !resolved.local.Alive {
			return fmt.Errorf("session %s is %s", resolved.local.ID, resolved.local.State())
		}
		return err
	}
	if Hibernation(resolved.remote) != nil {
		return fmt.Errorf("session %s on %s is already hibernated", resolved.remote.ID, resolved.host.Alias)
	}
	if !liveState(resolved.remote.State) {
		return fmt.Errorf("session %s on %s is %s", resolved.remote.ID, resolved.host.Alias, resolved.remote.State)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Second)
	defer cancel()
	return hibernateRemote(ctx, *resolved.host, a.dependencies.DialHost, resolved.remote.ID, 0)
}

func liveState(state string) bool {
	return state == string(storage.StateRunning) || state == string(storage.StateDetached)
}

// hibernateLocal asks the worker directly, as Kill does, so it works while the
// daemon is down. A positive idle makes the worker re-check its own clocks.
func hibernateLocal(s Session, idle time.Duration) error {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	response, err := exchangeWorkerControl(s, protocol.Control{
		Type: protocol.TypeHibernate, RequestID: requestID, SessionID: s.ID,
		HibernateIdleMillis: idle.Milliseconds(),
	})
	if err != nil {
		return err
	}
	switch response.Type {
	case protocol.TypeOK:
		return nil
	case protocol.TypeError:
		return hibernationRefused(s.ID, response.Message)
	default:
		return fmt.Errorf("hibernate %s: unexpected completion", s.ID)
	}
}

func hibernateRemote(ctx context.Context, host HostRecord, dial HostDialer, sessionID string, idle time.Duration) error {
	label := sessionID + " on " + host.Alias
	response, err := remoteSessionControl(ctx, host, dial, protocol.Control{
		Type: protocol.TypeHibernate, SessionID: sessionID, HibernateIdleMillis: idle.Milliseconds(),
	})
	if err != nil {
		return err
	}
	switch response.Type {
	case protocol.TypeOK:
		if response.SessionID != sessionID {
			return fmt.Errorf("host %s acknowledged a different session", host.Alias)
		}
		return nil
	case protocol.TypeError:
		return hibernationRefused(label, response.Message)
	default:
		return fmt.Errorf("host %s returned an unexpected %s response", host.Alias, protocol.TypeHibernate)
	}
}

// hibernationRefused turns a worker or daemon refusal into "session X: why".
// The worker's answer already names the session, and a refusal such as "not
// detached long enough" is an answer, not a fault worth a stack of prefixes.
func hibernationRefused(label, message string) error {
	reason := strings.TrimPrefix(message, "worker: ")
	if rest, ok := strings.CutPrefix(reason, "session "); ok {
		if id, after, found := strings.Cut(rest, ": "); found && !strings.Contains(id, " ") {
			reason = after
		}
	}
	reason = strings.TrimPrefix(reason, "daemon: ")
	switch {
	case reason == "expected "+protocol.TypeAttach:
		reason = "its worker predates hibernation; kill it instead, or leave it running"
	case strings.HasPrefix(reason, "unknown control"):
		reason = "its host predates hibernation; update Mesh there"
	case reason == "":
		reason = "hibernation was refused"
	}
	return fmt.Errorf("session %s: %s", label, safeRemoteText(reason))
}

// exchangeWorkerControl sends one request to a local worker and returns its
// reply without judging it, so each caller can phrase the refusal itself.
func exchangeWorkerControl(s Session, msg protocol.Control) (protocol.Control, error) {
	conn, err := net.DialTimeout("unix", paths.Socket(s.Dir), 2*time.Second)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("%s %s: %w", msg.Type, s.ID, err)
	}
	defer conn.Close() //nolint:errcheck // one-shot connection
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return protocol.Control{}, fmt.Errorf("%s %s: set completion deadline: %w", msg.Type, s.ID, err)
	}
	if err := protocol.NewWriter(conn).WriteControlMsg(msg); err != nil {
		return protocol.Control{}, fmt.Errorf("%s %s: %w", msg.Type, s.ID, err)
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return protocol.Control{}, fmt.Errorf("%s %s: worker closed without confirming completion", msg.Type, s.ID)
		}
		return protocol.Control{}, fmt.Errorf("%s %s: read completion: %w", msg.Type, s.ID, err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("%s %s: completion frame has kind %d", msg.Type, s.ID, frame.Kind)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, presentError(msg.Type+" "+s.ID+": decode completion", err)
	}
	// A worker that predates the request answers as if to a bad attach, with
	// no request ID to match.
	if response.Type == protocol.TypeError && response.RequestID == "" && response.SessionID == s.ID {
		return response, nil
	}
	if response.RequestID != msg.RequestID || response.SessionID != s.ID {
		return protocol.Control{}, fmt.Errorf("%s %s: mismatched completion", msg.Type, s.ID)
	}
	return response, nil
}

// attachOrWake attaches by ID, resuming a hibernated conversation instead of
// reporting the session exited, and recovering an interrupted one the way the
// picker does.
func (a *application) attachOrWake(cmd *cobra.Command, resolved resolvedSession, detachKey string, raw bool) error {
	if resolvedInterrupted(resolved) {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "session %s was interrupted; recovering it…\n", resolvedLabel(resolved)); err != nil {
			return err
		}
		return a.recoverSession(cmd, resolved, recovery.ActionDefault, detachKey, raw, false)
	}
	if marker := resolvedHibernation(resolved); marker != nil {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "resuming hibernated %s conversation…\n", marker.Provider); err != nil {
			return err
		}
		return a.recoverSession(cmd, resolved, recovery.ActionDefault, detachKey, raw, false)
	}
	return a.wakeIfHibernating(cmd, resolved, a.attachResolved(cmd, resolved, detachKey, raw, nil), detachKey, raw)
}

// wakeIfHibernating turns an attachment that raced a stopping worker into a
// wake, and passes every other attach result through.
func (a *application) wakeIfHibernating(cmd *cobra.Command, resolved resolvedSession, err error, detachKey string, raw bool) error {
	if !errors.Is(err, ErrSessionHibernating) {
		return err
	}
	// The worker was stopping its agent as we arrived. Once it is gone the
	// saved recipe resumes the conversation, so one retry through recovery
	// turns the race into the wake the user asked for.
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "session is hibernating; resuming its conversation…"); err != nil {
		return err
	}
	settled, err := a.awaitHibernated(cmd.Context(), resolved)
	if err != nil {
		return err
	}
	return a.recoverSession(cmd, settled, recovery.ActionDefault, detachKey, raw, false)
}

// awaitHibernated polls until the hibernating worker has exited, returning the
// session as it now reads so recovery sees an ended source.
func (a *application) awaitHibernated(parent context.Context, resolved resolvedSession) (resolvedSession, error) {
	ctx, cancel := context.WithTimeout(parent, hibernationSettle)
	defer cancel()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, ended, err := a.reread(ctx, resolved)
		if err != nil {
			return resolvedSession{}, err
		}
		if ended {
			return current, nil
		}
		select {
		case <-ctx.Done():
			id := resolved.remote.ID
			if resolved.local != nil {
				id = resolved.local.ID
			}
			return resolvedSession{}, fmt.Errorf("session %s is still hibernating after %s; attach again to resume it", id, hibernationSettle)
		case <-ticker.C:
		}
	}
}

func (a *application) reread(ctx context.Context, resolved resolvedSession) (resolvedSession, bool, error) {
	if resolved.local != nil {
		current, err := Find(resolved.local.ID)
		if err != nil {
			return resolvedSession{}, false, err
		}
		return resolvedSession{local: &current}, !current.Alive, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
	rows, err := a.queryHost(queryCtx, *resolved.host)
	cancel()
	if err != nil {
		return resolvedSession{}, false, err
	}
	current, err := exactRecoveryTarget(*resolved.host, rows, resolved.remote.ID)
	if err != nil {
		return resolvedSession{}, false, err
	}
	return current, !liveState(current.remote.State), nil
}

// latestHibernated is the fallback for `mesh HOST -r`: the most recently
// active hibernated session, used only when nothing on the host is live.
func latestHibernated(rows []protocol.SessionInfo) (protocol.SessionInfo, bool) {
	var (
		latest protocol.SessionInfo
		found  bool
	)
	for _, row := range rows {
		if Hibernation(row) == nil {
			continue
		}
		if !found || row.LastActiveAt().After(latest.LastActiveAt()) {
			latest, found = row, true
		}
	}
	return latest, found
}
