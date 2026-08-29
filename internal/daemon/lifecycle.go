package daemon

import (
	"context"
	"fmt"
	"strings"
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
	Catalog     lifecycleCatalog
	Connector   WorkerConnector
	Host        storage.Host
	SessionsDir string
	Executable  string
	Env         []string
	Launch      launchWorker
}

type lifecycle struct {
	catalog     lifecycleCatalog
	connector   WorkerConnector
	host        storage.Host
	sessionsDir string
	executable  string
	env         []string
	launch      launchWorker
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
	cfg.Host.Alias = cloneLifecycleString(cfg.Host.Alias)
	cfg.Host.TailscaleName = cloneLifecycleString(cfg.Host.TailscaleName)
	return &lifecycle{
		catalog:     cfg.Catalog,
		connector:   cfg.Connector,
		host:        cfg.Host,
		sessionsDir: cfg.SessionsDir,
		executable:  cfg.Executable,
		env:         append([]string(nil), cfg.Env...),
		launch:      cfg.Launch,
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

func (l *lifecycle) create(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if len(request.Command) == 0 || request.Command[0] == "" {
		return protocol.Control{}, fmt.Errorf("daemon: %s request has no command", request.Type)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}
	launched, err := l.launch(worker.LaunchConfig{
		SessionsDir: l.sessionsDir,
		Executable:  l.executable,
		Command:     append([]string(nil), request.Command...),
		Cwd:         request.Cwd,
		Env:         append([]string(nil), l.env...),
		Cols:        request.Cols,
		Rows:        request.Rows,
	})
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	parsedID, err := session.ParseID(launched.Meta.ID)
	if err != nil || parsedID != launched.Meta.ID {
		return protocol.Control{}, fmt.Errorf("daemon: launcher returned invalid session ID %q", launched.Meta.ID)
	}
	// The worker now exists independently of this client. Finish publishing it
	// even if the request connection disappeared while launch was in progress.
	if err := l.catalog.Reconcile(context.WithoutCancel(ctx)); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: publish session %s: %w", launched.Meta.ID, err)
	}
	return protocol.Control{
		Type:      protocol.TypeCreated,
		RequestID: request.RequestID,
		SessionID: launched.Meta.ID,
	}, nil
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
	sid, err := protocol.NewSessionID(id)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: encode session ID %s: %w", id, err)
	}
	conn, err := l.connector.ConnectWorker(ctx, sid)
	if err != nil {
		return protocol.Control{}, err
	}
	forwarded := protocol.Control{
		Type:      request.Type,
		RequestID: request.RequestID,
		SessionID: id,
	}
	if request.Type == protocol.TypeSignal {
		forwarded.Signal = request.Signal
	}
	payload, err := forwarded.Encode()
	if err == nil {
		err = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	}
	closeErr := conn.Close()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: forward %s to session %s: %w", request.Type, id, err)
	}
	if closeErr != nil {
		return protocol.Control{}, fmt.Errorf("daemon: close session %s control connection: %w", id, closeErr)
	}
	return protocol.Control{
		Type:      protocol.TypeOK,
		RequestID: request.RequestID,
		SessionID: id,
	}, nil
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
