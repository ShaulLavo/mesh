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

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

type lifecycleCatalog interface {
	Reconcile(context.Context) error
	List(context.Context) ([]storage.Session, error)
	Get(context.Context, storage.SessionID) (storage.Session, error)
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
		context:        cfg.Context,
		catalog:        cfg.Catalog,
		connector:      cfg.Connector,
		host:           cfg.Host,
		privateName:    cfg.PrivateName,
		sessionsDir:    cfg.SessionsDir,
		executable:     cfg.Executable,
		env:            append([]string(nil), cfg.Env...),
		launch:         cfg.Launch,
		publishTimeout: cfg.PublishTimeout,
		creations:      make(map[string]*creation),
	}, nil
}

// HandleControl handles daemon-owned control requests. Attachment controls are
// left to clientRelay, and unknown controls are left to the caller to reject.
func (l *lifecycle) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	switch request.Type {
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
	case protocol.TypeSignal, protocol.TypeKill:
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
			Executable:  l.executable,
			Command:     append([]string(nil), created.request.command...),
			Cwd:         created.request.cwd,
			Env:         append([]string(nil), l.env...),
			Cols:        created.request.cols,
			Rows:        created.request.rows,
			Term:        created.request.term,
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
			ID:            string(l.host.ID),
			MeshIdentity:  l.host.MeshIdentity,
			TailscaleName: name,
			PrivateName:   l.privateName(),
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
	if err == nil && (request.Type == protocol.TypeKill || request.Type == protocol.TypeLogs) {
		var frame protocol.Frame
		frame, err = conn.ReadFrame()
		if err == nil && request.Type == protocol.TypeKill {
			err = validateKillAcknowledgement(id, request.RequestID, frame)
		} else if err == nil {
			response, err = validateLogsResponse(id, request.RequestID, request.Tail, frame)
		}
	}
	closeErr := conn.Close()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: forward %s to session %s: %w", request.Type, id, err)
	}
	if closeErr != nil {
		return protocol.Control{}, fmt.Errorf("daemon: close session %s control connection: %w", id, closeErr)
	}
	return response, nil
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
