package mcpnative

import (
	"context"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
)

const (
	jobKindPipe     = "pipe"
	jobKindTerminal = "pty"
)

// TerminalControls run terminal jobs an agent created. They reach only keyed
// terminal jobs owned by this application identity, never a person's own
// session, and the worker refuses input and resizes while a person is
// attached.
type TerminalControls interface {
	TerminalOpen(ctx context.Context, req TerminalOpenRequest) (CallOutcome, error)
	TerminalResize(ctx context.Context, req TerminalResizeRequest) (CallOutcome, error)
	TerminalScreen(ctx context.Context, req TerminalScreenRequest) (CallOutcome, error)
}

// ServiceControls change routes, list apps and run the non-interactive
// session lifecycle controls, with each host's own restrictions.
type ServiceControls interface {
	StartService(ctx context.Context, req ServiceRouteRequest) (CallOutcome, error)
	StopService(ctx context.Context, req ServiceRouteRequest) (CallOutcome, error)
	PublishService(ctx context.Context, req PublishServiceRequest) (CallOutcome, error)
	UnpublishService(ctx context.Context, req ServiceRouteRequest) (CallOutcome, error)
	ListApps(ctx context.Context, req ListAppsRequest) (CallOutcome, error)
	HibernateSession(ctx context.Context, req SessionLifecycleRequest) (CallOutcome, error)
	RecoverSession(ctx context.Context, req SessionLifecycleRequest) (CallOutcome, error)
}

type TerminalOpenRequest struct {
	Context    ContextRef
	RequestKey string
	Command    string
	Argv       []string
	Cwd        string
	Env        map[string]string
	Cols, Rows int
	Term       string
	Timeout    time.Duration
}

type TerminalResizeRequest struct {
	Job        JobRef
	Context    *ContextRef
	Cols, Rows int
	Timeout    time.Duration
}

type TerminalScreenRequest struct {
	Job        JobRef
	Context    *ContextRef
	Cols, Rows int
	Timeout    time.Duration
}

// TerminalScreenData is the structured result of mesh_terminal_screen.
type TerminalScreenData struct {
	Job    string                  `json:"job"`
	Target TargetMeta              `json:"target"`
	Screen agentapi.TerminalScreen `json:"screen"`
}

// TerminalSizeData is the structured result of mesh_terminal_resize.
type TerminalSizeData struct {
	Job    string                `json:"job"`
	Target TargetMeta            `json:"target"`
	Size   agentapi.TerminalSize `json:"size"`
}

// ServiceRouteRequest names one route on one host.
type ServiceRouteRequest struct {
	Host    string
	Name    string
	Timeout time.Duration
}

// PublishServiceRequest carries the options of mesh serve that make sense
// without a terminal.
type PublishServiceRequest struct {
	Host           string
	Target         string
	Route          string
	Label          string
	Run            string
	Cwd            string
	Env            []string
	Listens        []string
	KeepRunning    bool
	IdleMS         *int
	ReadyTimeoutMS *int
	Files          bool
	Isolate        bool
	Timeout        time.Duration
}

type ListAppsRequest struct {
	Host    string
	Timeout time.Duration
}

// SessionLifecycleRequest names one session for hibernation or recovery.
type SessionLifecycleRequest struct {
	Host      string
	SessionID string
	Action    string
	Timeout   time.Duration
}
