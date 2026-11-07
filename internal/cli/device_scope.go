package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
)

const deviceRevokeVerb = "revoke"
const scopeListVerb = "list"

func (a *application) deviceScopeCommand() *cobra.Command {
	group := &cobra.Command{Use: "scope", Short: "Manage dedicated metadata-only TLS identities", RunE: func(*cobra.Command, []string) error {
		return agentapi.Usage("scope.list", "Choose keygen, grant, revoke, list or read.")
	}}
	group.Flags().Bool("json", false, "write one agent result")
	group.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		private, _ := cmd.Root().PersistentFlags().GetBool("privacy")
		if private {
			return agentapi.Usage(a.agent.operation, "Run scope commands without privacy mode.")
		}
		structured, _ := cmd.Flags().GetBool("json")
		if !structured {
			return agentapi.Usage(a.agent.operation, "Scope commands require --json.")
		}
		return nil
	}
	group.AddCommand(a.scopeKeygenCommand(), a.scopeGrantCommand(), a.scopeRevokeCommand(), a.scopeListCommand(), a.scopeReadCommand())
	return group
}
func (a *application) scopeKeygenCommand() *cobra.Command {
	var directory string
	command := &cobra.Command{Use: "keygen", Short: "Create or load a key in an explicit private directory", Args: profileArgs("scope.keygen"), RunE: func(cmd *cobra.Command, _ []string) error {
		if directory == "" {
			return agentapi.Usage("scope.keygen", "Pass --key-dir with a dedicated private directory.")
		}
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return fmt.Errorf("resolve credential directory: %w", err)
		}
		host, err := identity.CreateCredential(absolute)
		if err != nil {
			return fmt.Errorf("create dedicated credential: %w", err)
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: agentapi.ScopeCredential{Identity: host.ID, KeyDirectory: absolute}})
	}}
	command.Flags().StringVar(&directory, "key-dir", "", "dedicated credential directory; never the normal Mesh key")
	command.Flags().Bool("json", false, "write one agent result")
	return command
}
func localScopeStore() (*access.Store, string, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, "", fmt.Errorf("locate scope policy: %w", err)
	}
	host, err := identity.LoadOwned(state)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", agentapi.Fail(agentapi.HostUninitialized, "", "", "This target has no existing Mesh identity.")
	}
	if err != nil {
		return nil, "", fmt.Errorf("read target identity: %w", err)
	}
	store, err := access.Open(state, host.ID, nil)
	if err != nil {
		return nil, "", fmt.Errorf("open target scope policy: %w", err)
	}
	return store, state, nil
}
func (a *application) scopeGrantCommand() *cobra.Command {
	var subject, permissions, absolute string
	var duration time.Duration
	var revision uint64
	command := &cobra.Command{Use: "grant", Short: "Grant metadata access locally at an expected policy revision", Args: profileArgs("scope.grant"), RunE: func(cmd *cobra.Command, _ []string) error {
		if subject == "" || permissions != "metadata" || !cmd.Flags().Changed("expected-revision") {
			return agentapi.Usage("scope.grant", "Pass --identity, --permissions metadata and --expected-revision.")
		}
		expiry, err := scopeExpiry(cmd, absolute, duration)
		if err != nil {
			return err
		}
		store, state, err := localScopeStore()
		if err != nil {
			return err
		}
		change, err := store.Grant(cmd.Context(), revision, access.GrantInput{Subject: access.Subject(subject), ExpiresAt: expiry}, scopeRoles(state))
		return a.writeScopeChange(cmd, change, err)
	}}
	command.Flags().StringVar(&subject, "identity", "", "dedicated public identity")
	command.Flags().StringVar(&permissions, "permissions", "", "metadata only; no terminal or output access")
	command.Flags().StringVar(&absolute, "expires-at", "", "absolute RFC3339 expiry on this target")
	command.Flags().DurationVar(&duration, "expires-in", 24*time.Hour, "target-clock lifetime, at most seven days")
	command.Flags().Uint64Var(&revision, "expected-revision", 0, "current local scope policy revision")
	command.Flags().Bool("json", false, "write one agent result")
	return command
}
func scopeExpiry(cmd *cobra.Command, absolute string, duration time.Duration) (time.Time, error) {
	if cmd.Flags().Changed("expires-at") {
		if cmd.Flags().Changed("expires-in") {
			return time.Time{}, agentapi.Usage("scope.grant", "Use only one expiry flag.")
		}
		expiry, err := time.Parse(time.RFC3339Nano, absolute)
		if err != nil {
			return time.Time{}, agentapi.Usage("scope.grant", "--expires-at requires an RFC3339 timestamp.")
		}
		return expiry.UTC(), nil
	}
	if duration <= 0 || duration > 7*24*time.Hour {
		return time.Time{}, agentapi.Usage("scope.grant", "--expires-in must be positive and at most seven days.")
	}
	return time.Now().UTC().Add(duration), nil
}
func scopeRoles(state string) access.ObserveOtherRoles {
	return func(ctx context.Context, subject access.Subject) (access.OtherRoles, error) {
		if err := ctx.Err(); err != nil {
			return access.OtherRoles{}, fmt.Errorf("scope grant cancelled: %w", err)
		}
		full, err := identity.DeviceMembership(state, string(subject))
		if err != nil {
			return access.OtherRoles{}, fmt.Errorf("observe full role: %w", err)
		}
		administrator, err := update.AdministratorMembership(state, string(subject))
		if err != nil {
			return access.OtherRoles{}, fmt.Errorf("observe updater role: %w", err)
		}
		return access.OtherRoles{FullDevice: full, UpdateAdministrator: administrator}, nil
	}
}
func (a *application) scopeRevokeCommand() *cobra.Command {
	var subject string
	var revision uint64
	command := &cobra.Command{Use: deviceRevokeVerb, Short: "Remove a scoped identity locally at an expected revision", Args: profileArgs("scope.revoke"), RunE: func(cmd *cobra.Command, _ []string) error {
		if subject == "" || !cmd.Flags().Changed("expected-revision") {
			return agentapi.Usage("scope.revoke", "Pass --identity and --expected-revision.")
		}
		store, _, err := localScopeStore()
		if err != nil {
			return err
		}
		change, err := store.Revoke(cmd.Context(), revision, access.Subject(subject))
		return a.writeScopeChange(cmd, change, err)
	}}
	command.Flags().StringVar(&subject, "identity", "", "scoped public identity to remove")
	command.Flags().Uint64Var(&revision, "expected-revision", 0, "current local scope policy revision")
	command.Flags().Bool("json", false, "write one agent result")
	return command
}
func scopeEntry(entry access.Entry) agentapi.ScopeEntry {
	return agentapi.ScopeEntry{Subject: string(entry.Subject), Incarnation: entry.Incarnation, Permissions: append([]string{}, entry.Permissions...), CreatedAt: entry.CreatedAt, ExpiresAt: entry.ExpiresAt}
}
func (a *application) writeScopeChange(cmd *cobra.Command, change access.Change, err error) error {
	result := agentapi.ScopeChange{Revision: change.Revision, Changed: change.Changed}
	if change.Entry != nil {
		entry := scopeEntry(*change.Entry)
		result.Entry = &entry
	}
	if err == nil {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: result})
	}
	var visible *access.VisibleUnconfirmed
	if !errors.As(err, &visible) {
		return err
	}
	failure := agentapi.Fail(agentapi.StateUnavailable, a.agent.operation, "", err.Error())
	return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusPartial, Data: result, Errors: []agentapi.Error{failure.Err}})
}
func (a *application) scopeListCommand() *cobra.Command {
	command := &cobra.Command{Use: scopeListVerb, Short: "Read this target's scope policy without contacting a daemon", Args: profileArgs("scope.list"), RunE: func(cmd *cobra.Command, _ []string) error {
		store, _, err := localScopeStore()
		if err != nil {
			return err
		}
		policy, err := store.List(cmd.Context())
		if err != nil {
			return fmt.Errorf("list scope policy: %w", err)
		}
		result := agentapi.ScopePolicy{Version: policy.Version, Target: policy.Target, Revision: policy.Revision, Entries: make([]agentapi.ScopeEntry, 0, len(policy.Entries))}
		for _, entry := range policy.Entries {
			result.Entries = append(result.Entries, scopeEntry(entry))
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: result})
	}}
	command.Flags().Bool("json", false, "write one agent result")
	return command
}
func (a *application) scopeReadCommand() *cobra.Command {
	var target, directory, section, sessionID, cursor string
	var limit int
	var timeout time.Duration
	command := &cobra.Command{Use: "read", Short: "Read one metadata page using only an explicit key and direct TLS", Args: profileArgs("scope.read"), RunE: func(cmd *cobra.Command, _ []string) error {
		if target == "" || directory == "" || timeout < 100*time.Millisecond || timeout > 60*time.Second {
			return agentapi.Usage("scope.read", "Pass --host, --key-dir and a timeout from 100ms to 60s.")
		}
		requestID, err := agentapi.NewRequestID()
		if err != nil {
			return fmt.Errorf("identify scope request: %w", err)
		}
		read := protocol.AccessRead{Section: section, SessionID: sessionID, Limit: limit, Cursor: cursor}
		request := protocol.AccessRequest{Type: protocol.TypeAccessRead, RequestID: requestID, Read: read}
		data, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("encode scoped request: %w", err)
		}
		if _, err := protocol.DecodeAccessRead(data); err != nil {
			return agentapi.Usage("scope.read", err.Error())
		}
		// Cobra presence matters even when omitempty would erase a supplied zero.
		if err := validateScopeReadFlags(cmd, read); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		result, err := readScopeMetadata(ctx, target, directory, requestID, data, section)
		if err != nil {
			return err
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: scopeMetadataResult(result)})
	}}
	command.Flags().StringVar(&target, "host", "", "exact saved host ID with an existing destination pin")
	command.Flags().StringVar(&directory, "key-dir", "", "explicit dedicated credential directory")
	command.Flags().StringVar(&section, "section", "host", "host, sessions or services")
	command.Flags().StringVar(&sessionID, "session", "", "exact session ID for the sessions section")
	command.Flags().StringVar(&cursor, "cursor", "", "continuation from this credential and section")
	command.Flags().IntVar(&limit, "limit", 0, "page size from 1 to 500; default 100")
	command.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "hard deadline from 100ms to 60s")
	command.Flags().Bool("json", false, "write one agent result")
	return command
}
func validateScopeReadFlags(cmd *cobra.Command, read protocol.AccessRead) error {
	if cmd.Flags().Changed("session") && (read.Section != "sessions" || read.SessionID == "") {
		return agentapi.Usage("scope.read", "--session requires a nonempty exact session ID in the sessions section.")
	}
	if cmd.Flags().Changed("cursor") && read.Cursor == "" {
		return agentapi.Usage("scope.read", "--cursor requires a nonempty continuation.")
	}

	if cmd.Flags().Changed("limit") && (read.Limit < 1 || read.Section == "host") {
		return agentapi.Usage("scope.read", "--limit requires a row section and a value from 1 to 500.")
	}
	if read.Section == "host" && (cmd.Flags().Changed("session") || cmd.Flags().Changed("cursor")) {
		return agentapi.Usage("scope.read", "Host metadata accepts no selectors.")
	}
	return nil
}
func readScopeMetadata(ctx context.Context, target, directory, requestID string, payload []byte, section string) (protocol.AccessReadResult, error) {
	subject, key, err := identity.LoadCredential(directory)
	if err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("load dedicated credential: %w", err)
	}
	host, err := scopeTarget(target)
	if err != nil {
		return protocol.AccessReadResult{}, err
	}
	conn, err := transport.DialOnce(ctx, host.Endpoint, transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: target, Admission: transport.ScopedAdmission}})
	if err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("scoped TLS connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("write scoped request: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("read scoped response: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.AccessReadResult{}, errors.New("scoped response is not a control frame")
	}
	return validateScopeReply(frame, requestID, subject.ID, target, section)
}
func scopeTarget(target string) (HostRecord, error) {
	config, err := loadHostConfig()
	if err != nil {
		return HostRecord{}, err
	}
	var host HostRecord
	for _, candidate := range config.Hosts {
		if candidate.ID == target {
			host = candidate
		}
	}
	if host.ID == "" || host.MeshIdentity != target {
		return HostRecord{}, agentapi.Fail(agentapi.HostUnknown, "scope.read", target, "Pass an exact saved host ID with its matching destination pin.")
	}

	return host, nil
}
func validateScopeReply(frame protocol.Frame, requestID, subject, target, section string) (protocol.AccessReadResult, error) {
	reply, err := protocol.DecodeAccessReply(frame.Payload)
	if err != nil {
		return protocol.AccessReadResult{}, fmt.Errorf("decode scoped response: %w", err)
	}
	if reply.RequestID != requestID {
		return protocol.AccessReadResult{}, errors.New("scoped reply belongs to another request")
	}
	if reply.Type == protocol.TypeAccessDenied {
		return protocol.AccessReadResult{}, agentapi.Fail(agentapi.Code(reply.ErrorCode), "scope.read", target, reply.Message)
	}
	result := *reply.Result
	if result.Subject.Principal != subject || result.Subject.TargetID != target || result.Section != section {
		return result, errors.New("scoped reply binding differs from the authenticated request")
	}
	return result, nil
}
func scopeMetadataResult(result protocol.AccessReadResult) agentapi.ScopeMetadata {
	subject := result.Subject
	data := agentapi.ScopeMetadata{Subject: agentapi.ScopeSubject{TargetID: subject.TargetID, Principal: subject.Principal, Class: subject.Class, Permissions: append([]string{}, subject.Permissions...), ExpiresAt: subject.ExpiresAt}, Section: result.Section, ObservedAt: result.ObservedAt, Freshness: result.Freshness, Snapshot: result.Snapshot, NextPage: result.NextPage, Sessions: []agentapi.ScopeSession{}, Services: []agentapi.ScopeService{}}
	if result.Host != nil {
		data.Host = &agentapi.ScopeHost{ID: result.Host.ID, MachineName: result.Host.MachineName, Version: result.Host.Version}
	}
	for _, row := range result.Sessions {
		data.Sessions = append(data.Sessions, agentapi.ScopeSession{ID: row.ID, HostID: row.HostID, State: row.State, CreatedAt: row.CreatedAt, LastAttachedAt: row.LastAttachedAt, DetachedAt: row.DetachedAt, ExitCode: row.ExitCode})
	}
	for _, row := range result.Services {
		data.Services = append(data.Services, agentapi.ScopeService{Name: row.Name, Kind: row.Kind, Scope: row.Scope, Configured: row.Configured})
	}
	return data
}
