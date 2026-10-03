package daemon

import (
	"container/list"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"

	inspectionwire "github.com/shaul/mesh/internal/inspection"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	terminalstate "github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/worker"
)

type lifecycleCatalog interface {
	Reconcile(context.Context) error
	List(context.Context) ([]storage.Session, error)
	Get(context.Context, storage.SessionID) (storage.Session, error)
	Remove(context.Context, storage.SessionID) error
}

type launchWorker func(worker.LaunchConfig) (worker.Launched, error)

type lifecycleConfig struct {
	Names               *machinename.Store
	NameChanged         func(protocol.HostInfo)
	Context             context.Context
	Catalog             lifecycleCatalog
	Connector           WorkerConnector
	Host                storage.Host
	PrivateName         func() string
	SessionsDir         string
	Executable          string
	Env                 []string
	Launch              launchWorker
	PublishTimeout      time.Duration
	OperationTimeout    time.Duration
	Now                 func() time.Time
	CreationRetention   time.Duration
	MaxPendingCreations int
	MaxCreationBytes    int64
}

type lifecycle struct {
	names            *machinename.Store
	nameChanged      func(protocol.HostInfo)
	renameMu         sync.Mutex
	context          context.Context
	catalog          lifecycleCatalog
	connector        WorkerConnector
	host             storage.Host
	privateName      func() string
	sessionsDir      string
	executable       string
	env              []string
	launch           launchWorker
	publishTimeout   time.Duration
	operationTimeout time.Duration
	// observeTerminalSize is a read-only compatibility boundary for workers
	// whose inspection protocol predates structured terminal styles.
	observeTerminalSize func(int) (int, int, bool)

	creationsMu         sync.Mutex
	creations           map[string]*creation
	completedCreations  list.List
	pendingCreations    int
	creationBytes       int64
	now                 func() time.Time
	creationRetention   time.Duration
	maxPendingCreations int
	maxCreationBytes    int64

	memory memorySampler
}

const (
	defaultPublishTimeout = 30 * time.Second
	// CLI creates have a 40-second budget. Bootstrap commands also guard their
	// operations with a durable journal, since they outlive daemon receipts.
	defaultCreationRetention = 10 * time.Minute
	// Thousands of simultaneous launches exceed normal terminal and app demand.
	defaultMaxPendingCreations = 4096
	defaultMaxCreationBytes    = 64 << 20
	creationReceiptBytes       = 512
	maxCreationErrorBytes      = 4096

	// Kill and hibernate acknowledgements follow the worker's five-second grace.
	defaultWorkerOperationTimeout = 15 * time.Second
)

type creationRequest struct {
	command []string
	cwd     string
	cols    int
	rows    int
	term    string
	depth   int
	// label and env are set only by the daemon itself, for sessions it
	// starts on a route's behalf; a client request never carries them.
	label string
	env   []string
}

func (r creationRequest) fingerprint() ([sha256.Size]byte, int64) {
	h := sha256.New()
	var size int64
	var encoded [binary.MaxVarintLen64]byte
	writeInt := func(value int) {
		n := binary.PutVarint(encoded[:], int64(value))
		_, _ = h.Write(encoded[:n])
	}
	writeString := func(value string) {
		writeInt(len(value))
		_, _ = h.Write([]byte(value))
		size += int64(len(value))
	}
	writeInt(len(r.command))
	for _, arg := range r.command {
		writeString(arg)
		size += 16
	}
	writeString(r.cwd)
	writeInt(r.cols)
	writeInt(r.rows)
	writeString(r.term)
	writeInt(r.depth)
	writeString(r.label)
	writeInt(len(r.env))
	for _, variable := range r.env {
		writeString(variable)
		size += 16
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest, size
}

type creation struct {
	requestID string
	digest    [sha256.Size]byte
	done      chan struct{}
	sessionID string
	launchErr error

	publishGate chan struct{}
	published   bool

	retainedBytes      int64
	expiresAt          time.Time
	catalogSeen        bool
	retirementObserved bool
	completed          *list.Element
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
	if cfg.OperationTimeout < 0 {
		return nil, fmt.Errorf("daemon: negative worker operation timeout")
	}
	if cfg.OperationTimeout == 0 {
		cfg.OperationTimeout = defaultWorkerOperationTimeout
	}
	var err error
	cfg, err = creationDefaults(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Host.Alias = cloneLifecycleString(cfg.Host.Alias)
	cfg.Host.TailscaleName = cloneLifecycleString(cfg.Host.TailscaleName)
	return &lifecycle{
		names:               cfg.Names,
		nameChanged:         cfg.NameChanged,
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
		operationTimeout:    cfg.OperationTimeout,
		observeTerminalSize: worker.ReadSessionLeaderTerminalSize,
		creations:           make(map[string]*creation),
		now:                 cfg.Now,
		creationRetention:   cfg.CreationRetention,
		maxPendingCreations: cfg.MaxPendingCreations,
		maxCreationBytes:    cfg.MaxCreationBytes,
	}, nil
}

func creationDefaults(cfg lifecycleConfig) (lifecycleConfig, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CreationRetention < 0 || cfg.MaxPendingCreations < 0 || cfg.MaxCreationBytes < 0 {
		return cfg, fmt.Errorf("daemon: negative creation retention or admission limit")
	}
	if cfg.CreationRetention == 0 {
		cfg.CreationRetention = defaultCreationRetention
	}
	if cfg.MaxPendingCreations == 0 {
		cfg.MaxPendingCreations = defaultMaxPendingCreations
	}
	if cfg.MaxCreationBytes == 0 {
		cfg.MaxCreationBytes = defaultMaxCreationBytes
	}
	return cfg, nil
}

// HandleControl handles daemon-owned control requests. Attachment controls are
// left to clientRelay, and unknown controls are left to the caller to reject.
func (l *lifecycle) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type == protocol.TypeCreate || request.Type == protocol.TypeRecover {
		if err := updategate.Check(filepath.Dir(l.sessionsDir)); err != nil {
			return protocol.Control{}, true, err
		}
	}
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
	case protocol.TypeHostRename:
		response, err := l.renameHost(ctx, request)
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
	case protocol.TypeInspect:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.inspect(ctx, request)
		return response, true, err
	case protocol.TypeSignal, protocol.TypeKill, protocol.TypeHibernate:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := l.forwardOneShot(ctx, request, l.connector.ConnectWorker)
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
		response, err := l.forwardOneShot(ctx, request, l.connector.ConnectWorker)
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
	id, err := l.createSession(ctx, request.Type, request.RequestID, creationRequest{
		command: append([]string(nil), request.Command...),
		cwd:     request.Cwd,
		cols:    request.Cols,
		rows:    request.Rows,
		term:    request.Term,
		depth:   request.Depth,
	})
	if err != nil {
		return protocol.Control{}, err
	}
	return protocol.Control{Type: protocol.TypeCreated, RequestID: request.RequestID, SessionID: id}, nil
}

// publicationError is a creation whose worker launched and is ready but which
// the catalog has not recorded yet. Its next reconcile lists the session.
type publicationError struct{ err error }

func (e publicationError) Error() string { return e.err.Error() }
func (e publicationError) Unwrap() error { return e.err }

// createSession returns the launched session's ID even when a later step
// fails: that worker may be running, and whoever asked for it owns it.
func (l *lifecycle) createSession(ctx context.Context, requestType, requestID string, wanted creationRequest) (string, error) {
	created, owner, err := l.creation(requestID, wanted)
	if err != nil {
		return "", err
	}
	var ownerLaunchErr error
	if owner {
		defer l.completeCreation(created)
		env := append([]string(nil), l.env...)
		if len(wanted.env) > 0 {
			if env == nil {
				env = os.Environ()
			}
			// Later entries win when the worker starts the command, so the
			// recipe overrides the daemon's own value of the same name.
			env = append(env, wanted.env...)
		}
		launched, launchErr := l.launch(worker.LaunchConfig{
			SessionsDir: l.sessionsDir,
			HostID:      string(l.host.ID),
			Executable:  l.executable,
			Command:     append([]string(nil), wanted.command...),
			Cwd:         wanted.cwd,
			Env:         env,
			Cols:        wanted.cols,
			Rows:        wanted.rows,
			Term:        wanted.term,
			Depth:       wanted.depth,
			Label:       wanted.label,
		})
		ownerLaunchErr = launchErr
		switch {
		case launchErr != nil:
			created.launchErr = errors.New(boundedCreationError(launchErr.Error()))
			var started *worker.StartedError
			if errors.As(launchErr, &started) && isCanonicalSessionID(started.ID) {
				created.sessionID = strings.Clone(started.ID)
			}
		case isCanonicalSessionID(launched.Meta.ID):
			created.sessionID = strings.Clone(launched.Meta.ID)
		default:
			created.launchErr = errors.New(boundedCreationError(fmt.Sprintf("launcher returned invalid session ID %q", boundedCreationError(launched.Meta.ID))))
		}
		close(created.done)
	} else {
		select {
		case <-created.done:
		case <-ctx.Done():
			return "", fmt.Errorf("daemon: wait for %s request %s: %w", requestType, requestID, ctx.Err())
		}
	}
	if created.launchErr != nil {
		launchErr := created.launchErr
		if ownerLaunchErr != nil {
			launchErr = ownerLaunchErr
		}
		return created.sessionID, fmt.Errorf("daemon: %s: %w", requestType, launchErr)
	}
	id := created.sessionID
	waitCtx := ctx
	if owner {
		waitCtx = l.context
	}
	select {
	case <-created.publishGate:
		defer func() { created.publishGate <- struct{}{} }()
	case <-waitCtx.Done():
		return id, publicationError{fmt.Errorf("daemon: wait to publish session %s: %w", id, waitCtx.Err())}
	}
	if !created.published {
		// Publication belongs to the daemon, not the disposable client, but must
		// still stop promptly with daemon shutdown and has its own upper bound.
		publishCtx, cancel := context.WithTimeout(l.context, l.publishTimeout)
		err = l.catalog.Reconcile(publishCtx)
		if err == nil {
			l.observePublishedCreation(publishCtx, created)
		}
		cancel()
		if err != nil {
			return id, publicationError{fmt.Errorf("daemon: publish session %s: %w", id, err)}
		}
		created.published = true
	}
	return id, nil
}

func (l *lifecycle) observePublishedCreation(ctx context.Context, created *creation) {
	if _, err := l.catalog.Get(ctx, storage.SessionID(created.sessionID)); err != nil {
		return
	}
	l.creationsMu.Lock()
	created.catalogSeen = true
	l.creationsMu.Unlock()
}

func (l *lifecycle) creation(requestID string, wanted creationRequest) (*creation, bool, error) {
	digest, requestBytes := wanted.fingerprint()
	l.creationsMu.Lock()
	defer l.creationsMu.Unlock()
	l.expireCreations()
	if existing := l.creations[requestID]; existing != nil {
		if existing.digest != digest {
			return nil, false, fmt.Errorf("daemon: request ID %q was already used for a different session creation", requestID)
		}
		return existing, false, nil
	}
	if l.pendingCreations >= l.maxPendingCreations {
		return nil, false, fmt.Errorf("daemon: create request %q exceeds pending creation limit (%d)", requestID, l.maxPendingCreations)
	}
	// Reserve error space before launching, so even a failed launch can leave a
	// replay receipt without overrunning the retained-byte budget.
	retainedBytes := creationReceiptBytes + int64(len(requestID)) + requestBytes + session.IDLen + maxCreationErrorBytes
	if retainedBytes > l.maxCreationBytes-l.creationBytes {
		return nil, false, fmt.Errorf("daemon: create request %q exceeds retained creation bytes limit (%d)", requestID, l.maxCreationBytes)
	}
	created := &creation{
		requestID:     strings.Clone(requestID),
		digest:        digest,
		done:          make(chan struct{}),
		publishGate:   make(chan struct{}, 1),
		retainedBytes: retainedBytes,
	}
	created.publishGate <- struct{}{}
	l.creations[created.requestID] = created
	l.pendingCreations++
	l.creationBytes += retainedBytes
	return created, true, nil
}

func boundedCreationError(message string) string {
	if len(message) > maxCreationErrorBytes {
		message = message[:maxCreationErrorBytes]
	}
	return strings.Clone(message)
}

func (l *lifecycle) completeCreation(created *creation) {
	l.creationsMu.Lock()
	defer l.creationsMu.Unlock()
	retainedBytes := int64(creationReceiptBytes + len(created.requestID) + len(created.sessionID))
	if created.launchErr != nil {
		retainedBytes += int64(len(created.launchErr.Error()))
	}
	l.creationBytes += retainedBytes - created.retainedBytes
	created.retainedBytes = retainedBytes
	l.pendingCreations--
	created.expiresAt = l.now().Add(l.creationRetention)
	created.completed = l.completedCreations.PushBack(created)
}

// An unpublished worker may be absent from the catalog for its entire life.
// Only a positive exit observation can start its receipt's retirement window.
func (l *lifecycle) expireCreations() {
	now := l.now()
	first := l.completedCreations.Front()
	if first == nil || now.Before(first.Value.(*creation).expiresAt) {
		return
	}
	lookupCtx, cancel := context.WithTimeout(l.context, l.operationTimeout)
	defer cancel()
	for element := l.completedCreations.Front(); element != nil; element = l.completedCreations.Front() {
		created := element.Value.(*creation)
		if now.Before(created.expiresAt) {
			return
		}
		if l.retainCreation(lookupCtx, created) {
			created.expiresAt = l.now().Add(l.creationRetention)
			l.completedCreations.MoveToBack(element)
			if lookupCtx.Err() != nil {
				return
			}
			continue
		}
		l.creationBytes -= created.retainedBytes
		delete(l.creations, created.requestID)
		l.completedCreations.Remove(element)
	}
}

func (l *lifecycle) retainCreation(ctx context.Context, created *creation) bool {
	if created.sessionID == "" {
		return false
	}
	if !l.creationEnded(ctx, created) {
		created.retirementObserved = false
		return true
	}
	if !created.retirementObserved {
		created.retirementObserved = true
		return true
	}
	return false
}

func (l *lifecycle) creationEnded(ctx context.Context, created *creation) bool {
	stored, err := l.catalog.Get(ctx, storage.SessionID(created.sessionID))
	if err == nil {
		created.catalogSeen = true
		return stored.State == storage.StateExited || stored.State == storage.StateInterrupted
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if created.catalogSeen {
		return true
	}
	_, ended := l.sessionExit(created.sessionID)
	return ended
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
	sizes := l.memory.sizesFor(l.sessionsDir, sessions, time.Now())
	for i, stored := range sessions {
		items[i] = sessionInfo(stored)
		items[i].MemoryBytes = sizes[items[i].ID]
		l.addRecoveryInfo(&items[i], request.Lean)
	}
	return protocol.Control{Type: protocol.TypeListed, RequestID: request.RequestID, Sessions: items}, nil
}

func (l *lifecycle) hostInfo(request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	host := l.declaredHostInfo()
	return protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &host}, nil
}

func (l *lifecycle) declaredHostInfo() protocol.HostInfo {
	tailscaleName := ""
	if l.host.TailscaleName != nil {
		tailscaleName = *l.host.TailscaleName
	}
	host := protocol.HostInfo{
		Build: executingBuild(), UpdateSupported: true, RecoverySupported: true, ServiceHealthSupported: true,
		ID: string(l.host.ID), MeshIdentity: l.host.MeshIdentity, TailscaleName: tailscaleName, PrivateName: l.privateName(),
	}
	if l.names != nil {
		claim := l.names.Current()
		host.MachineName = claim.MachineName
		host.NameRevision = claim.Revision
	}
	return host
}

func (l *lifecycle) renameHost(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if ctx == nil {
		return protocol.Control{}, fmt.Errorf("daemon: rename has nil context")
	}
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if l.names == nil || request.Rename == nil {
		return protocol.Control{}, fmt.Errorf("daemon: rename requires a destination name and revision")
	}
	// Persist and enqueue in the same order, including retries after a lost reply.
	l.renameMu.Lock()
	defer l.renameMu.Unlock()
	rename := request.Rename
	_, _, err := l.names.Rename(ctx, rename.TargetID, rename.MachineName, rename.ExpectedRevision)
	// Recovery may finish a pending durable commit before rejecting this request.
	host := l.declaredHostInfo()
	if l.nameChanged != nil {
		l.nameChanged(host)
	}
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: rename machine: %w", err)
	}
	return protocol.Control{
		Type: protocol.TypeHostRenamed, RequestID: request.RequestID, Host: &host,
		Message: "Name saved on this machine. Other devices see it when they reconnect. Unreachable devices may know another machine by this name.",
	}, nil
}

// forwardOneShot sends one validated control to a worker reached through
// connect: the catalog-checked connector for clients, or connectOwned for a
// session the daemon launched itself.
func (l *lifecycle) forwardOneShot(ctx context.Context, request protocol.Control, connect func(context.Context, protocol.SessionID) (transport.Conn, error)) (protocol.Control, error) {
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
	if request.Type == protocol.TypeHibernate {
		if _, err := worker.HibernateIdleDuration(request.HibernateIdleMillis); err != nil {
			return protocol.Control{}, fmt.Errorf("daemon: hibernate session %s: %w", id, err)
		}
	}
	sid, err := protocol.NewSessionID(id)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: encode session ID %s: %w", id, err)
	}
	ctx, cancel := workerOperationContext(ctx, l.context, l.operationTimeout)
	defer cancel()
	conn, err := connect(ctx, sid)
	if err != nil {
		return protocol.Control{}, workerOperationError(ctx, sid, "connect worker for "+request.Type, err)
	}
	defer func() { _ = conn.Close() }()
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
	} else if request.Type == protocol.TypeHibernate {
		forwarded.HibernateIdleMillis = request.HibernateIdleMillis
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
	if err == nil && (request.Type == protocol.TypeKill || request.Type == protocol.TypeLogs || request.Type == protocol.TypeInspect || request.Type == protocol.TypeHibernate) {
		var frame protocol.Frame
		frame, err = conn.ReadFrame()
		if err == nil {
			switch request.Type {
			case protocol.TypeKill:
				err = validateKillAcknowledgement(id, request.RequestID, frame)
			case protocol.TypeHibernate:
				err = validateHibernateAcknowledgement(id, request.RequestID, frame)
			case protocol.TypeLogs:
				response, err = validateLogsResponse(id, request.RequestID, request.Tail, frame)
			case protocol.TypeInspect:
				response, err = validateInspectionResponse(id, request.RequestID, request.PreviewCols, request.PreviewRows, frame)
			}
		}
	}
	closeErr := conn.Close()
	var refused workerRefusal
	if errors.As(err, &refused) {
		return protocol.Control{}, refused
	}
	if err != nil {
		return protocol.Control{}, workerOperationError(ctx, sid, "forward "+request.Type, err)
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
	}, l.connector.ConnectWorker)
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

// workerRefusal is a worker's own answer to a request it declined. The daemon
// returns it unwrapped: "not detached long enough" is an answer, not a fault.
type workerRefusal struct{ message string }

func (r workerRefusal) Error() string { return r.message }

// validateHibernateAcknowledgement accepts a refusal without the request ID
// because a worker that predates hibernation answers every unknown request
// with an ID-less "expected session.attach", which the client explains.
func validateHibernateAcknowledgement(id, requestID string, frame protocol.Frame) error {
	if frame.Kind != protocol.KindControl {
		return fmt.Errorf("daemon: session %s hibernation acknowledgement has kind %d", id, frame.Kind)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("daemon: session %s hibernation acknowledgement: %w", id, err)
	}
	if message.SessionID != id {
		return fmt.Errorf("daemon: session %s invalid hibernation acknowledgement", id)
	}
	switch {
	case message.Type == protocol.TypeError:
		return workerRefusal{message: message.Message}
	case message.Type == protocol.TypeOK && message.RequestID == requestID:
		return nil
	default:
		return fmt.Errorf("daemon: session %s invalid hibernation acknowledgement", id)
	}
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
	if err := l.catalog.Remove(ctx, storage.SessionID(id)); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s %s: %w", request.Type, id, err)
	}
	// TypeOK, like kill and signal: remove travels through the same client
	// helper, and a result type of its own would make that helper special-case
	// one control.
	return protocol.Control{Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: id}, nil
}
