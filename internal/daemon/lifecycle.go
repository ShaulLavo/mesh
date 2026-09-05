package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"

	inspectionwire "github.com/shaul/mesh/internal/inspection"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	terminalstate "github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/worker"
)

type lifecycleCatalog interface {
	Reconcile(context.Context) error
	List(context.Context) ([]storage.Session, error)
	Get(context.Context, storage.SessionID) (storage.Session, error)
	Retire(context.Context, []storage.SessionID) (int64, error)
}

type launchWorker func(worker.LaunchConfig) (worker.Launched, error)

type lifecycleConfig struct {
	Context        context.Context
	Catalog        lifecycleCatalog
	Connector      WorkerConnector
	Host           storage.Host
	PrivateName    func() string
	SessionsDir    string
	Executable     string
	Env            []string
	Launch         launchWorker
	PublishTimeout time.Duration
}

type lifecycle struct {
	context        context.Context
	catalog        lifecycleCatalog
	connector      WorkerConnector
	host           storage.Host
	privateName    func() string
	sessionsDir    string
	executable     string
	env            []string
	launch         launchWorker
	publishTimeout time.Duration
	// observeTerminalSize is a read-only compatibility boundary for workers
	// whose inspection protocol predates structured terminal styles.
	observeTerminalSize func(int) (int, int, bool)

	creationsMu sync.Mutex
	creations   map[string]*creation
}

const defaultPublishTimeout = 30 * time.Second

type creationRequest struct {
	command []string
	cwd     string
	cols    int
	rows    int
	term    string
	depth   int
}

func (r creationRequest) equal(other creationRequest) bool {
	return slices.Equal(r.command, other.command) && r.cwd == other.cwd && r.cols == other.cols && r.rows == other.rows
}

type creation struct {
	request creationRequest
	done    chan struct{}

	launched  worker.Launched
	launchErr error

	publishGate chan struct{}
	published   bool
}

func newLifecycle(cfg lifecycleConfig) (*lifecycle, error) {
	if cfg.Catalog == nil {
		return nil, fmt.Errorf("daemon: nil lifecycle catalog")
	}
	if cfg.Connector == nil {
		return nil, fmt.Errorf("daemon: nil lifecycle worker connector")
	}
	if strings.TrimSpace(string(cfg.Host.ID)) == "" || strings.TrimSpace(cfg.Host.MeshIdentity) == "" {
		return nil, fmt.Errorf("daemon: incomplete host identity")
	}
	if cfg.SessionsDir == "" {
		return nil, fmt.Errorf("daemon: empty sessions directory")
	}
	if cfg.Launch == nil {
		cfg.Launch = worker.LaunchDetached
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	if cfg.PrivateName == nil {
		cfg.PrivateName = func() string { return "" }
	}
	if cfg.PublishTimeout < 0 {
		return nil, fmt.Errorf("daemon: negative lifecycle publication timeout")
	}
	if cfg.PublishTimeout == 0 {
		cfg.PublishTimeout = defaultPublishTimeout
	}
	cfg.Host.Alias = cloneLifecycleString(cfg.Host.Alias)
	cfg.Host.TailscaleName = cloneLifecycleString(cfg.Host.TailscaleName)
	return &lifecycle{
		context:             cfg.Context,
		catalog:             cfg.Catalog,
		connector:           cfg.Connector,
		host:                cfg.Host,
		privateName:         cfg.PrivateName,
		sessionsDir:         cfg.SessionsDir,
		executable:          cfg.Executable,
		env:                 append([]string(nil), cfg.Env...),
		launch:              cfg.Launch,
		publishTimeout:      cfg.PublishTimeout,
		observeTerminalSize: worker.ReadSessionLeaderTerminalSize,
		creations:           make(map[string]*creation),
	}, nil
}

// HandleControl handles daemon-owned control requests. Attachment controls are
// left to clientRelay, and unknown controls are left to the caller to reject.
func (l *lifecycle) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	switch request.Type {
	case protocol.TypeRecover, protocol.TypeRecoveryRead, protocol.TypeRecoveryCommand:
		response, err := l.recoveryControl(ctx, request)
		return response, true, err
	case protocol.TypeCreate:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.create(ctx, request)
		return response, true, err
	case protocol.TypeList:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.list(ctx, request)
		return response, true, err
	case protocol.TypeHostInfo:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.hostInfo(request)
		return response, true, err
	case protocol.TypeLogs:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.logs(ctx, request)
		return response, true, err
	case protocol.TypeRemove:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.remove(ctx, request)
		return response, true, err
	case protocol.TypeSignal, protocol.TypeKill, protocol.TypeInspect:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.forwardOneShot(ctx, request)
		return response, true, err
	default:
		return protocol.Control{}, false, nil
	}
}

func (l *lifecycle) logs(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if request.Tail <= 0 || request.Tail > protocol.MaxLogTail {
		return protocol.Control{}, fmt.Errorf("daemon: log tail must be between 1 and %d bytes", protocol.MaxLogTail)
	}
	id, err := session.ParseID(request.SessionID)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	stored, err := l.catalog.Get(ctx, storage.SessionID(id))
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if stored.State == storage.StateRunning || stored.State == storage.StateDetached {
		response, err := l.forwardOneShot(ctx, request)
		if err == nil {
			return response, nil
		}
		// The catalog is a hint. A session that exited moments ago still reads
		// as running until reconciliation notices, and its socket is already
		// gone, so fall through to the durable tail rather than reporting a
		// dial failure for output that is sitting on disk.
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
			return protocol.Control{}, err
		}
	}
	output, err := worker.ReadLogTail(filepath.Join(l.sessionsDir, id), request.Tail)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: logs for session %s: %w", id, err)
	}
	return protocol.Control{
		Type: protocol.TypeLogged, RequestID: request.RequestID, SessionID: id, Output: output,
	}, nil
}

func (l *lifecycle) create(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if len(request.Command) == 0 {
		request.Command = []string{hostShell()}
	}
	if request.Command[0] == "" {
		return protocol.Control{}, fmt.Errorf("daemon: %s request has an empty command", request.Type)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}
	created, owner, err := l.creation(request)
	if err != nil {
		return protocol.Control{}, err
	}
	if owner {
		created.launched, created.launchErr = l.launch(worker.LaunchConfig{
			SessionsDir: l.sessionsDir,
			HostID:      string(l.host.ID),
			Executable:  l.executable,
			Command:     append([]string(nil), created.request.command...),
			Cwd:         created.request.cwd,
			Env:         append([]string(nil), l.env...),
			Cols:        created.request.cols,
			Rows:        created.request.rows,
			Term:        created.request.term,
			Depth:       created.request.depth,
		})
		close(created.done)
	} else {
		select {
		case <-created.done:
		case <-ctx.Done():
			return protocol.Control{}, fmt.Errorf("daemon: wait for %s request %s: %w", request.Type, request.RequestID, ctx.Err())
		}
	}
	if created.launchErr != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, created.launchErr)
	}
	parsedID, err := session.ParseID(created.launched.Meta.ID)
	if err != nil || parsedID != created.launched.Meta.ID {
		return protocol.Control{}, fmt.Errorf("daemon: launcher returned invalid session ID %q", created.launched.Meta.ID)
	}
	waitCtx := ctx
	if owner {
		waitCtx = l.context
	}
	select {
	case <-created.publishGate:
		defer func() { created.publishGate <- struct{}{} }()
	case <-waitCtx.Done():
		return protocol.Control{}, fmt.Errorf("daemon: wait to publish session %s: %w", created.launched.Meta.ID, waitCtx.Err())
	}
	if !created.published {
		// Publication belongs to the daemon, not the disposable client, but must
		// still stop promptly with daemon shutdown and has its own upper bound.
		publishCtx, cancel := context.WithTimeout(l.context, l.publishTimeout)
		err = l.catalog.Reconcile(publishCtx)
		cancel()
		if err != nil {
			return protocol.Control{}, fmt.Errorf("daemon: publish session %s: %w", created.launched.Meta.ID, err)
		}
		created.published = true
	}
	return protocol.Control{
		Type:      protocol.TypeCreated,
		RequestID: request.RequestID,
		SessionID: created.launched.Meta.ID,
	}, nil
}

func (l *lifecycle) creation(request protocol.Control) (*creation, bool, error) {
	wanted := creationRequest{
		command: append([]string(nil), request.Command...),
		cwd:     request.Cwd,
		cols:    request.Cols,
		rows:    request.Rows,
		term:    request.Term,
		depth:   request.Depth,
	}
	l.creationsMu.Lock()
	defer l.creationsMu.Unlock()
	if existing := l.creations[request.RequestID]; existing != nil {
		if !existing.request.equal(wanted) {
			return nil, false, fmt.Errorf("daemon: request ID %q was already used for a different session creation", request.RequestID)
		}
		return existing, false, nil
	}
	created := &creation{
		request:     wanted,
		done:        make(chan struct{}),
		publishGate: make(chan struct{}, 1),
	}
	created.publishGate <- struct{}{}
	l.creations[request.RequestID] = created
	return created, true, nil
}

func (l *lifecycle) list(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	sessions, err := l.catalog.List(ctx)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	items := make([]protocol.SessionInfo, len(sessions))
	for i, stored := range sessions {
		items[i] = sessionInfo(stored)
		l.addRecoveryInfo(&items[i])
	}
	return protocol.Control{Type: protocol.TypeListed, RequestID: request.RequestID, Sessions: items}, nil
}

func (l *lifecycle) hostInfo(request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	name := ""
	if l.host.TailscaleName != nil {
		name = *l.host.TailscaleName
	}
	return protocol.Control{
		Type:      protocol.TypeHostInfoResult,
		RequestID: request.RequestID,
		Host: &protocol.HostInfo{
			RecoverySupported: true,
			ID:                string(l.host.ID),
			MeshIdentity:      l.host.MeshIdentity,
			TailscaleName:     name,
			PrivateName:       l.privateName(),
		},
	}, nil
}

func (l *lifecycle) forwardOneShot(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	id, err := session.ParseID(request.SessionID)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if request.Type == protocol.TypeSignal && !worker.SupportsSignal(request.Signal) {
		return protocol.Control{}, fmt.Errorf("daemon: unsupported signal %q", request.Signal)
	}
	if request.Type == protocol.TypeLogs && (request.Tail <= 0 || request.Tail > protocol.MaxLogTail) {
		return protocol.Control{}, fmt.Errorf("daemon: log tail must be between 1 and %d bytes", protocol.MaxLogTail)
	}
	if request.Type == protocol.TypeInspect {
		if err := protocol.ValidateInspectDimensions(request.PreviewCols, request.PreviewRows); err != nil {
			return protocol.Control{}, fmt.Errorf("daemon: inspect session %s: %w", request.SessionID, err)
		}
	}
	sid, err := protocol.NewSessionID(id)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: encode session ID %s: %w", id, err)
	}
	conn, err := l.connector.ConnectWorker(ctx, sid)
	if err != nil {
		return protocol.Control{}, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancellation()
	forwarded := protocol.Control{
		Type:      request.Type,
		RequestID: request.RequestID,
		SessionID: id,
	}
	if request.Type == protocol.TypeSignal {
		forwarded.Signal = request.Signal
	} else if request.Type == protocol.TypeLogs {
		forwarded.Tail = request.Tail
	} else if request.Type == protocol.TypeInspect {
		forwarded.PreviewCols = request.PreviewCols
		forwarded.PreviewRows = request.PreviewRows
	}
	payload, err := forwarded.Encode()
	if err == nil {
		err = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	}
	response := protocol.Control{
		Type:      protocol.TypeOK,
		RequestID: request.RequestID,
		SessionID: id,
	}
	if err == nil && (request.Type == protocol.TypeKill || request.Type == protocol.TypeLogs || request.Type == protocol.TypeInspect) {
		var frame protocol.Frame
		frame, err = conn.ReadFrame()
		if err == nil {
			switch request.Type {
			case protocol.TypeKill:
				err = validateKillAcknowledgement(id, request.RequestID, frame)
			case protocol.TypeLogs:
				response, err = validateLogsResponse(id, request.RequestID, request.Tail, frame)
			case protocol.TypeInspect:
				response, err = validateInspectionResponse(id, request.RequestID, request.PreviewCols, request.PreviewRows, frame)
			}
		}
	}
	closeErr := conn.Close()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: forward %s to session %s: %w", request.Type, id, err)
	}
	if closeErr != nil {
		return protocol.Control{}, fmt.Errorf("daemon: close session %s control connection: %w", id, closeErr)
	}
	if request.Type == protocol.TypeInspect {
		l.enrichLegacyInspection(ctx, id, request, &response)
	}
	return response, nil
}

// enrichLegacyInspection recovers presentation from the exact raw ANSI bytes
// exposed by an older worker. The old worker's plain inspection remains the
// authority: unless the replay reproduces every row, no recovered style is
// used. Any compatibility failure leaves the valid plain response untouched.
func (l *lifecycle) enrichLegacyInspection(ctx context.Context, id string, request protocol.Control, response *protocol.Control) {
	if response == nil || response.Inspection == nil || len(response.Inspection.Preview) == 0 ||
		len(response.Inspection.StyledPreview) != 0 || request.PreviewCols == 1 && request.PreviewRows == 1 ||
		l.observeTerminalSize == nil {
		return
	}
	meta, err := worker.ReadMeta(filepath.Join(l.sessionsDir, id))
	if err != nil || meta.ID != id || meta.PID <= 0 {
		return
	}
	screenCols, screenRows, ok := l.observeTerminalSize(meta.PID)
	if !ok {
		return
	}
	logs, err := l.forwardOneShot(ctx, protocol.Control{
		Type: protocol.TypeLogs, RequestID: request.RequestID, SessionID: id, Tail: protocol.MaxLogTail,
	})
	if err != nil || logs.Type != protocol.TypeLogged {
		return
	}
	replayed, err := terminalstate.PreviewANSIOutputAtSize(
		logs.Output,
		screenCols,
		screenRows,
		request.PreviewCols,
		request.PreviewRows,
	)
	if err != nil {
		return
	}
	styles, ok := terminalstate.MatchPreviewStyles(replayed, response.Inspection.Preview)
	if !ok {
		return
	}
	styled := inspectionwire.StyledPreview(terminalstate.Preview{
		Lines:       response.Inspection.Preview,
		StyledLines: styles,
	})
	if len(styled) == 0 {
		return
	}
	candidate := *response.Inspection
	candidate.StyledPreview = styled
	if err := protocol.ValidateSessionInspection(candidate); err != nil {
		return
	}
	response.Inspection = &candidate
}

func validateKillAcknowledgement(id, requestID string, frame protocol.Frame) error {
	if frame.Kind != protocol.KindControl {
		return fmt.Errorf("daemon: session %s kill acknowledgement has kind %d", id, frame.Kind)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("daemon: session %s kill acknowledgement: %w", id, err)
	}
	if message.Type != protocol.TypeOK || message.RequestID != requestID || message.SessionID != id {
		return fmt.Errorf("daemon: session %s invalid kill acknowledgement", id)
	}
	return nil
}

func validateLogsResponse(id, requestID string, tail int, frame protocol.Frame) (protocol.Control, error) {
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("daemon: session %s logs response has kind %d", id, frame.Kind)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: session %s logs response: %w", id, err)
	}
	if message.Type != protocol.TypeLogged || message.RequestID != requestID || message.SessionID != id {
		return protocol.Control{}, fmt.Errorf("daemon: session %s invalid logs response", id)
	}
	if len(message.Output) > tail {
		return protocol.Control{}, fmt.Errorf("daemon: session %s returned %d log bytes, want at most %d", id, len(message.Output), tail)
	}
	return message, nil
}

func validateInspectionResponse(id, requestID string, cols, rows int, frame protocol.Frame) (protocol.Control, error) {
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("daemon: session %s inspection response has kind %d", id, frame.Kind)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: session %s inspection response: %w", id, err)
	}
	if message.Type != protocol.TypeInspected || message.RequestID != requestID || message.SessionID != id || message.Inspection == nil {
		return protocol.Control{}, fmt.Errorf("daemon: session %s invalid inspection response", id)
	}
	if err := protocol.ValidateSessionInspection(*message.Inspection); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: session %s invalid inspection response: %w", id, err)
	}
	if len(message.Inspection.Preview) > rows {
		return protocol.Control{}, fmt.Errorf("daemon: session %s returned %d preview rows, want at most %d", id, len(message.Inspection.Preview), rows)
	}
	for row, line := range message.Inspection.Preview {
		if width := ansi.StringWidth(line); width > cols {
			return protocol.Control{}, fmt.Errorf("daemon: session %s returned preview row %d with width %d, want at most %d", id, row, width, cols)
		}
	}
	return message, nil
}

func validateRequestID(request protocol.Control) error {
	if strings.TrimSpace(request.RequestID) == "" {
		return fmt.Errorf("daemon: %s request has no request ID", request.Type)
	}
	return nil
}

func sessionInfo(stored storage.Session) protocol.SessionInfo {
	return protocol.SessionInfo{
		ID:                 string(stored.ID),
		HostID:             string(stored.HostID),
		Command:            append([]string(nil), stored.Command...),
		Cwd:                stored.Cwd,
		State:              string(stored.State),
		CreatedAt:          stored.CreatedAt,
		LastAttachedAt:     cloneLifecycleTime(stored.LastAttachedAt),
		ExitCode:           cloneLifecycleInt(stored.ExitCode),
		LastOutputSequence: stored.LastOutputSequence,
	}
}

func cloneLifecycleString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLifecycleTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLifecycleInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// hostShell is the shell a session gets when the client names no command. A
// remote client cannot choose this: its own $SHELL is a path on its own
// machine, which may be absent here or a different program entirely.
func hostShell() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	if shell := passwdShell(); shell != "" {
		return shell
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "/bin/sh"
}

// passwdShell reads the login shell for this uid. macOS keeps users in a
// directory service rather than /etc/passwd, so a miss here is ordinary.
func passwdShell() string {
	contents, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	uid := strconv.Itoa(os.Getuid())
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[2] != uid {
			continue
		}
		if shell := strings.TrimSpace(fields[6]); shell != "" {
			return shell
		}
	}
	return ""
}

// remove forgets a finished session and deletes the state it left behind. A
// running session is refused rather than killed: ending someone's work is a
// separate decision from tidying up after it, and `mesh kill` already makes it.
func (l *lifecycle) remove(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	id, err := session.ParseID(request.SessionID)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if err := l.catalog.Reconcile(ctx); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: reconcile before removing session %s: %w", id, err)
	}
	stored, err := l.catalog.Get(ctx, storage.SessionID(id))
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if stored.State == storage.StateRunning || stored.State == storage.StateDetached {
		return protocol.Control{}, fmt.Errorf("daemon: session %s is %s; kill it before removing it", id, stored.State)
	}
	removed, err := l.catalog.Retire(ctx, []storage.SessionID{storage.SessionID(id)})
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s %s: %w", request.Type, id, err)
	}
	if removed == 0 {
		return protocol.Control{}, fmt.Errorf("daemon: session %s was not removed; it may have restarted", id)
	}
	// The catalog row is gone, so a directory left here would be re-adopted by
	// the next reconciliation and the session would come back.
	if err := os.RemoveAll(filepath.Join(l.sessionsDir, id)); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: remove session directory %s: %w", id, err)
	}
	// TypeOK, like kill and signal: remove travels through the same client
	// helper, and a result type of its own would make that helper special-case
	// one control.
	return protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: id}, nil
}
