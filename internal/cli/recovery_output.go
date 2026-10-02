package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func (a *application) previousOutput(cmd *cobra.Command, id string, tail int) error {
	if a.privacy != nil {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Previous output withheld in privacy mode"); err != nil {
			return fmt.Errorf("show private previous output notice: %w", err)
		}
		return nil
	}
	resolved, err := a.resolveRecoverySession(cmd.Context(), id)
	if err != nil {
		return err
	}
	record, err := a.readRecoveryRecord(cmd.Context(), resolved)
	if err != nil {
		return err
	}
	text := strings.Join(record.Lines, "\n") + "\n"
	if len(text) > tail {
		start := len(text) - tail
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		text = text[start:]
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Previous output · %s\n%s", record.CheckpointAt.Format(time.RFC3339), text)
	return err
}

func (a *application) readRecoveryRecord(ctx context.Context, resolved resolvedSession) (recovery.Record, error) {
	if resolved.local != nil {
		return readLocalRecoveryRecord(*resolved.local)
	}
	ctx, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
	defer cancel()
	conn, err := openVerifiedHost(ctx, *resolved.host, a.dependencies.DialControl)
	if err != nil {
		return recovery.Record{}, err
	}
	defer func() { _ = conn.Close() }()
	id, err := newDaemonRequestID()
	if err != nil {
		return recovery.Record{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{
		Type: protocol.TypeRecoveryRead, RequestID: id, SessionID: resolved.remote.ID,
	})
	if err != nil {
		return recovery.Record{}, err
	}
	if response.Type != protocol.TypeRecoveryRecord || response.Recovery == nil || response.Recovery.CheckpointAt.IsZero() {
		return recovery.Record{}, errors.New("previous output is unavailable on this host")
	}
	return *response.Recovery, recovery.ValidateOwner(*response.Recovery, resolved.host.ID, resolved.remote.ID)
}

func readLocalRecoveryRecord(current Session) (recovery.Record, error) {
	record, err := recovery.Read(current.Dir)
	if err != nil {
		return recovery.Record{}, fmt.Errorf("previous output unavailable: %w", err)
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return recovery.Record{}, fmt.Errorf("locate recovery host: %w", err)
	}
	host, err := existingLocalIdentity(stateDir)
	if err != nil {
		return recovery.Record{}, err
	}
	if host.ID == "" {
		return recovery.Record{}, fmt.Errorf("session %s: previous output unavailable: local host identity is missing", current.ID)
	}
	if err := recovery.ValidateOwner(record, host.ID, current.ID); err != nil {
		return recovery.Record{}, fmt.Errorf("session %s: validate previous output owner: %w", current.ID, err)
	}
	return record, nil
}
