package agentapi

import (
	"encoding/json"
	"time"
)

// Hosted MCP catalog host states (PLAN sections 6.6 and 7.3).
const (
	MCPHostOnline          = "online"
	MCPHostStale           = "stale"
	MCPHostExpired         = "expired"
	MCPHostOffline         = "offline"
	MCPHostUnauthorized    = "unauthorized"
	MCPHostUnsupported     = "unsupported"
	MCPHostIdentityChanged = "identity_changed"
	MCPHostFailed          = "failed"
)

// MCPCatalogInstance is one caller-visible hosted MCP server instance.
type MCPCatalogInstance struct {
	ID           string          `json:"id"`
	Handle       string          `json:"handle,omitempty"`
	Shared       bool            `json:"shared"`
	Mine         bool            `json:"mine"`
	Default      bool            `json:"default,omitempty"`
	State        string          `json:"state"`
	Reason       string          `json:"reason,omitempty"`
	ExitCode     *int            `json:"exitCode,omitempty"`
	Revision     int64           `json:"revision"`
	Generation   int64           `json:"generation"`
	Active       int             `json:"active"`
	Leases       int             `json:"leases"`
	ServerInfo   json.RawMessage `json:"serverInfo,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	LateReplies  uint64          `json:"lateReplies,omitempty"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

// MCPCatalogService is one hosted MCP server in the fleet catalog or show result.
type MCPCatalogService struct {
	HostID         string               `json:"hostId"`
	MachineName    string               `json:"machineName,omitempty"`
	Local          bool                 `json:"local,omitempty"`
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	Revision       int64                `json:"revision"`
	State          string               `json:"state"`
	Freshness      string               `json:"freshness"`
	ObservedAt     *time.Time           `json:"observedAt,omitempty"`
	Transport      string               `json:"transport"`
	Command        []string             `json:"command,omitempty"`
	Cwd            string               `json:"cwd,omitempty"`
	URL            string               `json:"url,omitempty"`
	Lifecycle      string               `json:"lifecycle"`
	IdleSeconds    int64                `json:"idleSeconds,omitempty"`
	ReadySeconds   int64                `json:"readySeconds"`
	InstancePolicy string               `json:"instancePolicy"`
	AccountMode    string               `json:"accountMode"`
	AccountLabel   string               `json:"accountLabel,omitempty"`
	Disabled       bool                 `json:"disabled,omitempty"`
	EnvNames       []string             `json:"env,omitempty"`
	SecretNames    []string             `json:"secretEnv,omitempty"`
	HeaderNames    []string             `json:"secretHeaders,omitempty"`
	Instances      []MCPCatalogInstance `json:"instances"`
}

// MCPCatalogHost reports the live, cached, or error state of one host in mesh mcp ls.
type MCPCatalogHost struct {
	HostID             string     `json:"hostId"`
	MachineName        string     `json:"machineName,omitempty"`
	Local              bool       `json:"local,omitempty"`
	State              string     `json:"state"`
	Freshness          string     `json:"freshness"`
	ObservedAt         *time.Time `json:"observedAt,omitempty"`
	ServiceCount       int        `json:"serviceCount"`
	CachedServiceCount int        `json:"cachedServiceCount,omitempty"`
	Code               Code       `json:"code,omitempty"`
	Message            string     `json:"message,omitempty"`
}

// MCPCatalogData is the data payload for mesh mcp ls --json.
type MCPCatalogData struct {
	ObservedAt time.Time           `json:"observedAt"`
	Hosts      []MCPCatalogHost    `json:"hosts"`
	Services   []MCPCatalogService `json:"services"`
}

// MCPServiceInstancesData is the data payload for mesh mcp start|stop|restart --json.
type MCPServiceInstancesData struct {
	HostID      string               `json:"hostId"`
	MachineName string               `json:"machineName,omitempty"`
	Service     string               `json:"service"`
	Instances   []MCPCatalogInstance `json:"instances"`
}

// MCPServiceLogsData is the data payload for mesh mcp logs --json.
type MCPServiceLogsData struct {
	HostID      string `json:"hostId"`
	MachineName string `json:"machineName,omitempty"`
	Service     string `json:"service"`
	Instance    string `json:"instance,omitempty"`
	Stderr      string `json:"stderr"`
	Bytes       int    `json:"bytes"`
}

// MCPServiceRemoveData is the data payload for mesh mcp rm --json.
type MCPServiceRemoveData struct {
	HostID      string `json:"hostId"`
	MachineName string `json:"machineName,omitempty"`
	Service     string `json:"service"`
	Removed     bool   `json:"removed"`
}

const (
	envFileFlag           = "--secret-env"
	urlFlag               = "--url"
	headerFileFlag        = "--header-secret"
	runFlag               = "--run"
	runCwdFlag            = "--run-cwd"
	idleFlag              = "--idle"
	readyTimeoutFlag      = "--ready-timeout"
	accountModeFlag       = "--account-mode"
	accountLabelFlag      = "--account-label"
	serviceRefValue       = "HOST/NAME"
	handleFlagValue       = "HANDLE"
	instanceHandleSummary = "instance ID or mi1 handle"
)

var mcpServiceOperations = []Operation{
	{
		Name:        "mcp.publish",
		Command:     "mesh mcp publish",
		Args:        "NAME [-- COMMAND [ARG...]]",
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: hostFlag, Value: hostValue, Summary: "target host (defaults to local)"},
			{Name: cwdFlag, Value: pathFlagValue, Summary: "working directory on the target host for a stdio server"},
			{Name: envFlag, Value: nameEqualsValue, Summary: "plain environment variable (repeatable)"},
			{Name: envFileFlag, Value: "NAME=PATH", Summary: "secret file path read into NAME at launch (mode 0600)"},
			{Name: urlFlag, Value: "URL", Summary: "loopback Streamable HTTP endpoint of an HTTP server"},
			{Name: headerFileFlag, Value: "HEADER=PATH", Summary: "credential header secret file sent to the HTTP server"},
			{Name: runFlag, Value: "COMMAND", Summary: "command that starts the HTTP server at --url"},
			{Name: runCwdFlag, Value: pathFlagValue, Summary: "working directory for --run"},
			{Name: "--shared", Summary: "give every authorized caller one shared instance"},
			{Name: "--kept", Summary: "keep the server running instead of starting on demand"},
			{Name: idleFlag, Value: "D", Summary: "idle timeout for an on-demand instance (default 15m)"},
			{Name: readyTimeoutFlag, Value: "D", Summary: "readiness probe timeout (default 30s)"},
			{Name: accountModeFlag, Value: "MODE", Summary: "none or host-account"},
			{Name: accountLabelFlag, Value: textValue, Summary: "non-secret host account label shown to callers"},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Publish or update a hosted MCP server definition on a host.",
		Example: "mesh mcp publish repo-tools --cwd /work/app --json -- ./repo-mcp",
	},
	{
		Name:        "mcp.list",
		Command:     "mesh mcp ls",
		Implemented: true,
		Network:     true,
		Flags: []Flag{
			{Name: hostFlag, Value: hostValue, Summary: "limit listing to one machine name, exact host ID, or local"},
			{Name: identityFlag, Value: nameValue, Summary: "scope instances and private catalog cache to this application identity"},
			{Name: timeoutFlag, Value: "D", Summary: "hard deadline for fleet queries (default 2s)"},
			{Name: cachedFlag, Summary: "read private cached catalog entries without dialing hosts"},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "List hosted MCP servers across saved hosts without waking hosts or starting services.",
		Example: "mesh mcp ls --json",
	},
	{
		Name:        "mcp.show",
		Command:     "mesh mcp show",
		Args:        serviceRefValue,
		Implemented: true,
		Network:     true,
		Flags: []Flag{
			{Name: identityFlag, Value: nameValue, Summary: actAsIdentitySummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Show one hosted MCP server definition and its instances.",
		Example: "mesh mcp show local/repo-tools --json",
	},
	{
		Name:        "mcp.start",
		Command:     "mesh mcp start",
		Args:        serviceRefValue,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: instanceFlag, Value: handleFlagValue, Summary: instanceHandleSummary},
			{Name: identityFlag, Value: nameValue, Summary: actAsIdentitySummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Start a hosted MCP server instance and verify stateless 2026-07-28 readiness.",
		Example: "mesh mcp start local/repo-tools --json",
	},
	{
		Name:        "mcp.stop",
		Command:     "mesh mcp stop",
		Args:        serviceRefValue,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: instanceFlag, Value: handleFlagValue, Summary: "instance ID or mi1 handle; omit to stop all managed instances"},
			{Name: identityFlag, Value: nameValue, Summary: actAsIdentitySummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Stop a hosted MCP server's running instances while keeping its definition.",
		Example: "mesh mcp stop local/repo-tools --json",
	},
	{
		Name:        "mcp.restart",
		Command:     "mesh mcp restart",
		Args:        serviceRefValue,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: instanceFlag, Value: handleFlagValue, Summary: instanceHandleSummary},
			{Name: identityFlag, Value: nameValue, Summary: actAsIdentitySummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Replace a hosted MCP server instance with a fresh process generation.",
		Example: "mesh mcp restart local/repo-tools --json",
	},
	{
		Name:        "mcp.logs",
		Command:     "mesh mcp logs",
		Args:        serviceRefValue,
		Implemented: true,
		Network:     true,
		Flags: []Flag{
			{Name: instanceFlag, Value: handleFlagValue, Summary: instanceHandleSummary},
			{Name: identityFlag, Value: nameValue, Summary: actAsIdentitySummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Read a hosted MCP server instance's bounded private stderr ring.",
		Example: "mesh mcp logs local/repo-tools --json",
	},
	{
		Name:        "mcp.remove",
		Command:     "mesh mcp rm",
		Args:        serviceRefValue,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Remove a hosted MCP server definition and stop its instances.",
		Example: "mesh mcp rm local/repo-tools --json",
	},
}

func mcpServiceValueFlag(arg string) bool {
	switch arg {
	case envFileFlag, urlFlag, headerFileFlag, runFlag, runCwdFlag, idleFlag, readyTimeoutFlag, accountModeFlag, accountLabelFlag:
		return true
	}
	return false
}
