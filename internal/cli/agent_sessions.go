package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
	"github.com/spf13/cobra"
)

const (
	agentSessionListCommand = "list"
	agentSessionsList       = "sessions.list"
	agentSessionsInspect    = "sessions.inspect"
)

type agentSessionOptions struct {
	host                 string
	json                 bool
	timeout              time.Duration
	all, detail, preview bool
	limit                int
	page                 string
}

type agentSessionCursor struct {
	HostID    string    `json:"hostId"`
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

type agentSessionRow struct {
	info  protocol.SessionInfo
	facts storage.SessionFacts
}

type agentSessionSnapshot struct {
	rows      []agentSessionRow
	freshness agentapi.Freshness
	errors    []agentapi.Error
}

func (a *application) sessionCommand() *cobra.Command {
	group := &cobra.Command{Use: "session", Short: "Read sessions and run keyed detached commands", RunE: func(_ *cobra.Command, _ []string) error {
		return agentapi.Usage(agentSessionsList, "Choose mesh session list or inspect.")
	}, PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		private, err := cmd.Flags().GetBool("privacy")
		if err != nil {
			return fmt.Errorf("read privacy flag: %w", err)
		}
		if private {
			return agentapi.Usage(a.agent.operation, "Privacy mode hides machine data. Run agent commands without it.")
		}
		return nil
	}}
	group.Flags().Bool("json", false, "write one agent result")
	group.AddCommand(a.sessionListCommand(), a.sessionInspectCommand(), a.sessionCreateCommand(), a.sessionRequestCommand(), a.sessionWaitCommand(), a.sessionLogsCommand(), a.sessionSignalCommand(), a.sessionKillCommand(), a.sessionRemoveCommand())
	return group
}

func agentSessionFlags(command *cobra.Command, options *agentSessionOptions) {
	command.Flags().StringVar(&options.host, "host", "", "machine name or exact host ID, required")
	command.Flags().BoolVar(&options.json, "json", false, "write one agent result, required")
	command.Flags().DurationVar(&options.timeout, "timeout", 5*time.Second, "hard deadline for the read, from 100ms to 60s")
}

func (a *application) sessionListCommand() *cobra.Command {
	options := &agentSessionOptions{}
	command := &cobra.Command{Use: agentSessionListCommand, Short: "List one host's sessions with bounded pages", Args: profileArgs(agentSessionsList), RunE: func(cmd *cobra.Command, _ []string) error {
		if err := options.validate(agentSessionsList); err != nil {
			return err
		}
		cursor, err := parseAgentSessionCursor(options.page)
		if err != nil {
			return err
		}
		return a.runAgentSessionList(cmd, *options, cursor)
	}}
	agentSessionFlags(command, options)
	command.Flags().BoolVar(&options.all, "all", false, "include exited and interrupted sessions")
	command.Flags().BoolVar(&options.detail, "detail", false, "include launch command and working directory")
	command.Flags().IntVar(&options.limit, "limit", 100, "maximum sessions in this page, from 1 to 500")
	command.Flags().StringVar(&options.page, "page", "", "opaque nextPage token from this host")
	return command
}

func (a *application) sessionInspectCommand() *cobra.Command {
	options := &agentSessionOptions{}
	command := &cobra.Command{Use: "inspect SESSION_ID", Short: "Read live or saved session details without attaching", Long: "Read a session's launch metadata and live or saved inspection.\nWithout --preview, Mesh reads one cell internally and discards its preview.\nWith --preview, Mesh reads at most 80 columns by 24 rows. Neither changes\nterminal size or attachment.", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return agentapi.Usage(agentSessionsInspect, "Pass one exact session ID.")
		}
		id, err := session.ParseID(args[0])
		if err != nil || id != args[0] {
			return agentapi.Usage(agentSessionsInspect, "Pass one exact session ID.")
		}
		return nil
	}, RunE: func(cmd *cobra.Command, args []string) error {
		if err := options.validate(agentSessionsInspect); err != nil {
			return err
		}
		return a.runAgentSessionInspect(cmd, *options, args[0])
	}}
	agentSessionFlags(command, options)
	command.Flags().BoolVar(&options.preview, "preview", false, "include a control-free preview, at most 80 by 24")
	return command
}

func (o agentSessionOptions) validate(operation string) error {
	if !o.json || strings.TrimSpace(o.host) == "" {
		return agentapi.Usage(operation, "Pass --host HOST and --json.")
	}
	if o.timeout < 100*time.Millisecond || o.timeout > 60*time.Second {
		return agentapi.Usage(operation, "--timeout must be from 100ms to 60s.")
	}
	if operation == agentSessionsList && (o.limit < 1 || o.limit > 500) {
		return agentapi.Usage(operation, "--limit must be from 1 to 500.")
	}
	return nil
}

func parseAgentSessionCursor(token string) (*agentSessionCursor, error) {
	if token == "" {
		return nil, nil
	}
	invalid := agentapi.Usage(agentSessionsList, "--page must be a nextPage token from this host.")
	if len(token) > 1024 {
		return nil, invalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cursor agentSessionCursor
	if err := decoder.Decode(&cursor); err != nil {
		return nil, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, invalid
	}
	id, err := session.ParseID(cursor.ID)
	if err != nil || id != cursor.ID || cursor.HostID == "" || cursor.CreatedAt.IsZero() || cursor.CreatedAt.UnixMilli() < 0 {
		return nil, invalid
	}
	return &cursor, nil
}

func (a *application) agentSessionTarget(ctx context.Context, value string) (HostRecord, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	return resolveAgentHost(ctx, value, hosts, false, a.dependencies.DialPassive)
}

func (a *application) runAgentSessionList(cmd *cobra.Command, options agentSessionOptions, cursor *agentSessionCursor) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), options.timeout)
	defer cancel()
	host, err := a.agentSessionTarget(ctx, options.host)
	if err != nil {
		return err
	}
	if cursor != nil && cursor.HostID != host.ID {
		return agentapi.Usage(agentSessionsList, "--page belongs to another host.")
	}
	snapshot, err := a.loadAgentSessions(ctx, host, agentSessionsList)
	if err != nil {
		return err
	}
	page := agentSessionPage(host.ID, snapshot, options, cursor)
	return a.agent.write(cmd.OutOrStdout(), agentSessionEnvelope(page, snapshot.errors))
}

func agentSessionEnvelope(data any, diagnostics []agentapi.Error) agentapi.Envelope {
	status := agentapi.StatusOK
	if len(diagnostics) > 0 {
		status = agentapi.StatusPartial
	}
	return agentapi.Envelope{Status: status, Data: data, Errors: diagnostics}
}

func agentSessionPage(hostID string, snapshot agentSessionSnapshot, options agentSessionOptions, cursor *agentSessionCursor) agentapi.SessionPage {
	rows := make([]agentSessionRow, 0, len(snapshot.rows))
	for _, row := range snapshot.rows {
		if !options.all && !liveState(row.info.State) {
			continue
		}
		if cursor != nil && !afterAgentSessionCursor(row.info, *cursor) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].info.CreatedAt.Equal(rows[j].info.CreatedAt) {
			return rows[i].info.ID < rows[j].info.ID
		}
		return rows[i].info.CreatedAt.After(rows[j].info.CreatedAt)
	})
	page := agentapi.SessionPage{HostID: hostID, Freshness: snapshot.freshness, Sessions: make([]agentapi.SessionSummary, 0, min(options.limit, len(rows)))}
	if len(rows) > options.limit {
		last := rows[options.limit-1]
		encoded, _ := json.Marshal(agentSessionCursor{HostID: hostID, CreatedAt: last.info.CreatedAt.UTC(), ID: last.info.ID})
		token := base64.RawURLEncoding.EncodeToString(encoded)
		page.NextPage = &token
		rows = rows[:options.limit]
	}
	for _, row := range rows {
		page.Sessions = append(page.Sessions, agentSessionSummary(row, options.detail))
	}
	return page
}

func afterAgentSessionCursor(row protocol.SessionInfo, cursor agentSessionCursor) bool {
	return row.CreatedAt.Before(cursor.CreatedAt) || row.CreatedAt.Equal(cursor.CreatedAt) && row.ID > cursor.ID
}

func agentSessionSummary(observed agentSessionRow, detail bool) agentapi.SessionSummary {
	row, facts := observed.info, observed.facts
	value := agentapi.SessionSummary{ID: row.ID, HostID: row.HostID, Kind: "pty", State: row.State, Hibernated: facts.Hibernated, RecoveryPending: facts.RecoveryPending, Label: cleanAgentSessionText(facts.Label), CreatedAt: row.CreatedAt.UTC(), LastAttachedAt: agentSessionTime(row.LastAttachedAt), DetachedAt: agentSessionTime(facts.DetachedAt), ExitCode: cloneInt(row.ExitCode)}
	if detail {
		value.Command = slices.Clone(row.Command)
		value.Cwd = row.Cwd
	}
	return value
}

func agentSessionTime(at *time.Time) *time.Time {
	if at == nil || at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

func cleanAgentSessionText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			return -1
		}
		return r
	}, ansi.Strip(text))
}

func (a *application) loadAgentSessions(ctx context.Context, host HostRecord, operation string) (agentSessionSnapshot, error) {
	if host.local {
		rows, err := listAgentLocalSessions(ctx, host)
		if err != nil {
			return agentSessionSnapshot{}, err
		}
		if err := ctx.Err(); err != nil {
			return agentSessionSnapshot{}, fmt.Errorf("read passive local sessions: %w", err)
		}
		return agentSessionSnapshot{rows: liveAgentSessionRows(rows), freshness: agentapi.FreshnessLive}, nil
	}
	return a.loadAgentRemoteSessions(ctx, host, operation)
}

func (a *application) loadAgentRemoteSessions(ctx context.Context, host HostRecord, operation string) (agentSessionSnapshot, error) {
	cache, cacheErr := openAgentCatalogCache(ctx)
	if cache == nil {
		cache = &SQLiteCatalogCache{}
	}
	defer cache.Close() //nolint:errcheck // read errors are reported separately
	stored, readErr := cache.LoadObserved(ctx, host)
	cached, withheld := cachedAgentSessionRows(stored)
	if readErr == nil {
		readErr = validateCachedAgentSessions(host.ID, cached)
	}
	if readErr != nil {
		cached = nil
	}
	rows, err := listAgentSessions(ctx, host, a.dependencies.DialPassive)
	if err != nil {
		failure := agentError(err, operation, host.ID, agentapi.HostUnreachable)
		if failure.Code != agentapi.HostUnreachable || len(cached) == 0 {
			return agentSessionSnapshot{}, &agentapi.Failure{Err: failure}
		}
		snapshot := agentSessionSnapshot{rows: cached, freshness: agentapi.FreshnessCached, errors: []agentapi.Error{failure}}
		if withheld > 0 {
			snapshot.errors = append(snapshot.errors, agentapi.Error{Code: agentapi.StateUnavailable, Operation: operation, Target: host.ID, Retryable: true, Message: fmt.Sprintf("%d cached sessions on this machine were saved without their full details, so they are not shown. Reach the machine for a complete list.", withheld)})
		}
		return snapshot, nil
	}
	snapshot := agentSessionSnapshot{rows: liveAgentSessionRows(rows), freshness: agentapi.FreshnessLive}
	_, saveErr := cache.SaveObserved(ctx, host, rows)
	if err := errors.Join(cacheErr, readErr, saveErr); err != nil {
		snapshot.errors = append(snapshot.errors, agentError(err, operation, host.ID, agentapi.StateUnavailable))
	}
	return snapshot, nil
}

func liveAgentSessionRows(rows []protocol.SessionInfo) []agentSessionRow {
	result := make([]agentSessionRow, len(rows))
	for i, row := range rows {
		result[i] = agentSessionRow{info: row, facts: observedSessionFacts(row)}
	}
	return result
}

func cachedAgentSessionRows(rows []CachedSession) ([]agentSessionRow, int) {
	result := make([]agentSessionRow, 0, len(rows))
	for _, row := range rows {
		if row.Facts == nil {
			continue
		}
		result = append(result, agentSessionRow{info: row.Info, facts: *row.Facts})
	}
	return result, len(rows) - len(result)
}

func validateCachedAgentSessions(hostID string, rows []agentSessionRow) error {
	for _, row := range rows {
		if err := validateAgentSessionRow(hostID, row.info); err != nil {
			return err
		}
	}
	return nil
}

func listAgentSessions(ctx context.Context, host HostRecord, dial HostDialer) ([]protocol.SessionInfo, error) {
	conn, _, err := openAgentHostInfo(ctx, host, dial)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // read result takes precedence
	requestID, err := newDaemonRequestID()
	if err != nil {
		return nil, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeList, RequestID: requestID, Lean: true})
	if err != nil {
		return nil, err
	}
	if response.Type == protocol.TypeError {
		return nil, daemonResponseError("session list", response.Message, response.ErrorCode)
	}
	if response.Type != protocol.TypeListed {
		return nil, errors.New("host returned an unexpected session-list response")
	}
	if err := validateAgentSessionRows(host.ID, response.Sessions); err != nil {
		return nil, err
	}
	return response.Sessions, nil
}

func validateAgentSessionRow(hostID string, row protocol.SessionInfo) error {
	if row.HostID != hostID {
		return &hostIdentityError{label: hostID}
	}
	if err := validateDaemonSession(row); err != nil {
		return err
	}
	if len(row.Command) > recovery.MaxCommandArgs || len(row.Cwd) > recovery.MaxFieldBytes || len(row.Label) > recovery.MaxFieldBytes {
		return errors.New("session metadata exceeds text limits")
	}
	size := 0
	for _, arg := range row.Command {
		size += len(arg)
	}
	if size > recovery.MaxCommandBytes {
		return errors.New("session command exceeds text limit")
	}
	return nil
}

func (a *application) runAgentSessionInspect(cmd *cobra.Command, options agentSessionOptions, id string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), options.timeout)
	defer cancel()
	host, err := a.agentSessionTarget(ctx, options.host)
	if err != nil {
		return err
	}
	snapshot, err := a.loadAgentSessions(ctx, host, agentSessionsInspect)
	if err != nil {
		return err
	}
	row, found := findAgentSession(snapshot.rows, id)
	if !found && len(snapshot.errors) > 0 {
		return &agentapi.Failure{Err: snapshot.errors[0]}
	}
	if !found {
		return agentapi.Fail(agentapi.SessionNotFound, agentSessionsInspect, id, "No such session on this host.")
	}
	result := agentapi.SessionDetail{Session: agentSessionSummary(row, true), Freshness: snapshot.freshness, LiveState: "unavailable"}
	if snapshot.freshness == agentapi.FreshnessCached {
		return a.agent.write(cmd.OutOrStdout(), agentSessionEnvelope(result, snapshot.errors))
	}
	cols, lines := 1, 1
	if options.preview {
		cols, lines = 80, 24
	}
	inspection, err := a.inspectAgentSession(ctx, host, id, cols, lines)
	if err != nil {
		failure := agentError(err, agentSessionsInspect, id, agentapi.HostUnreachable)
		if failure.Code == agentapi.SessionNotFound || failure.Code == agentapi.HostIdentityMismatch || failure.Code == agentapi.HostAuthenticationFailed {
			return &agentapi.Failure{Err: failure}
		}
		snapshot.errors = append(snapshot.errors, failure)
	} else {
		applyAgentInspection(&result, inspection, options.preview)
	}
	return a.agent.write(cmd.OutOrStdout(), agentSessionEnvelope(result, snapshot.errors))
}

func findAgentSession(rows []agentSessionRow, id string) (agentSessionRow, bool) {
	for _, row := range rows {
		if row.info.ID == id {
			return row, true
		}
	}
	return agentSessionRow{}, false
}

func (a *application) inspectAgentSession(ctx context.Context, host HostRecord, id string, cols, rows int) (SessionInspection, error) {
	if host.local {
		return inspectAgentLocalSession(ctx, host, id, cols, rows)
	}
	conn, _, err := openAgentHostInfo(ctx, host, a.dependencies.DialPassive)
	if err != nil {
		return SessionInspection{}, err
	}
	defer conn.Close() //nolint:errcheck // read result takes precedence
	return inspectAgentConnection(ctx, conn, host, id, cols, rows)
}

func inspectAgentConnection(ctx context.Context, conn transport.Conn, host HostRecord, id string, cols, rows int) (SessionInspection, error) {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return SessionInspection{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeInspect, RequestID: requestID, SessionID: id, PreviewCols: cols, PreviewRows: rows})
	if err != nil {
		return SessionInspection{}, err
	}
	return validateInspectionResponse(host, id, cols, rows, response)
}

func inspectAgentLocalSession(ctx context.Context, host HostRecord, id string, cols, rows int) (SessionInspection, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return SessionInspection{}, fmt.Errorf("locate passive inspection state: %w", err)
	}
	dir := filepath.Join(state, "s", id)
	if _, err := os.Lstat(dir); err != nil {
		return SessionInspection{}, fmt.Errorf("read session directory: %w", err)
	}
	current, ok, err := readAgentLocalSession(ctx, dir)
	if err != nil {
		return SessionInspection{}, err
	}
	if !ok || current.ID != id {
		return SessionInspection{}, agentapi.Fail(agentapi.SessionNotFound, agentSessionsInspect, id, "No such session on this host.")
	}
	if !liveState(current.State()) {
		fallback := recovery.Record{Version: recovery.Version, HostID: host.ID, SessionID: id, Shell: defaultShell(), ShellDirectory: current.Cwd, DirectorySource: recovery.DirectoryLaunch, Command: current.Command}
		record, err := recovery.ReadSaved(dir, host.ID, id, fallback)
		if err != nil {
			return SessionInspection{}, fmt.Errorf("read saved inspection: %w", err)
		}
		return savedInspection(host, id, protocol.RecoveryPreview(record))
	}
	conn, err := dialAgentLocal(ctx, paths.Socket(dir))
	if err != nil {
		return SessionInspection{}, err
	}
	defer conn.Close() //nolint:errcheck // read result takes precedence
	return inspectAgentConnection(ctx, conn, host, id, cols, rows)
}

func applyAgentInspection(result *agentapi.SessionDetail, inspection SessionInspection, preview bool) {
	if record := inspection.Recovery; record != nil {
		saved := &agentapi.SavedInspection{CheckpointAt: agentSessionTime(&record.CheckpointAt), Directory: record.ShellDirectory, DirectorySource: string(record.DirectorySource), Command: slices.Clone(record.Command), TerminalTitle: cleanAgentSessionText(record.Title), LastOutputAt: agentSessionTime(&record.LastOutputAt), Preview: []string{}}
		if preview {
			saved.Preview = agentSavedPreview(record.Lines)
		}
		result.LiveState, result.Saved = "saved", saved
		return
	}
	live := &agentapi.LiveInspection{ObservedAt: inspection.ObservedAt.UTC(), Attached: inspection.Attached, CurrentDirectory: cleanAgentSessionText(inspection.CurrentDirectory), ForegroundCommand: cleanAgentSessionText(inspection.ForegroundCommand), TerminalTitle: cleanAgentSessionText(inspection.TerminalTitle), LastOutputAt: agentSessionTime(inspection.LastOutputAt)}
	if preview {
		live.Preview = agentSavedPreview(inspection.Preview)
	}
	result.LiveState, result.Live = "live", live
}

func agentSavedPreview(lines []string) []string {
	preview := make([]string, len(lines))
	for i, line := range lines {
		preview[i] = ansi.Truncate(cleanAgentSessionText(line), 80, "")
	}
	return preview
}

func listAgentLocalSessions(ctx context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate passive session state: %w", err)
	}
	root := filepath.Join(state, "s")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) && sessionDirectoryAbsent(root) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read passive session directory: %w", err)
	}
	rows := make([]protocol.SessionInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		current, ok, err := readAgentLocalSession(ctx, filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		row := agentLocalSessionRow(current, host.ID)
		if err := validateAgentSessionRow(host.ID, row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func agentLocalSessionRow(current Session, hostID string) protocol.SessionInfo {
	row := protocol.SessionInfo{ID: current.ID, HostID: hostID, Command: current.Command, Cwd: current.Cwd, State: current.State(), CreatedAt: current.CreatedAt, LastAttachedAt: current.LastAttachedAt, DetachedAt: current.DetachedAt, ExitCode: current.ExitCode, Label: current.Label}
	addLocalRecoveryInfo(&row, current, hostID)
	row.Hibernated = localHibernation(current, row.ReplacementID)
	return row
}

func readAgentLocalSession(ctx context.Context, dir string) (Session, bool, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, false, fmt.Errorf("read passive session: %w", err)
	}
	for _, marker := range []string{paths.Forgotten(dir), paths.Launching(dir)} {
		if _, err := os.Lstat(marker); err == nil {
			return Session{}, false, nil
		}
	}
	info, err := os.Stat(paths.Meta(dir))
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("stat passive session metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > recovery.MaxRecordBytes {
		return Session{}, false, errors.New("session metadata is not a bounded regular file")
	}
	meta, err := worker.ReadMeta(dir)
	if err != nil {
		return Session{}, false, fmt.Errorf("read passive session metadata: %w", err)
	}
	if meta.ID != filepath.Base(dir) {
		return Session{}, false, errors.New("session metadata belongs to a different directory")
	}
	liveness := agentLocalLiveness(ctx, dir, &meta)
	return Session{Meta: meta, Dir: dir, Liveness: liveness}, true, nil
}

func agentLocalLiveness(ctx context.Context, dir string, meta *worker.Meta) Liveness {
	if boot := worker.BootID(); meta.BootID != "" && boot != "" && meta.BootID != boot {
		return LivenessGone
	}
	conn, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, "unix", paths.Socket(dir))
	if err == nil {
		_ = conn.Close()
		return LivenessAlive
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return LivenessUnknown
	}
	refreshed, err := worker.ReadMeta(dir)
	if err != nil {
		return LivenessUnknown
	}
	*meta = refreshed
	return LivenessGone
}

func validateAgentSessionRows(hostID string, rows []protocol.SessionInfo) error {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if err := validateAgentSessionRow(hostID, row); err != nil {
			return err
		}
		if seen[row.ID] {
			return errors.New("host listed duplicate session IDs")
		}
		seen[row.ID] = true
	}
	return nil
}
