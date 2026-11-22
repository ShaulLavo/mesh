package agentapi

import "time"

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

// MCPServerInfo is the allowlisted upstream implementation identity in catalog views.
type MCPServerInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Title   string `json:"title,omitempty"`
}

// MCPListCapability is the allowlisted list-change capability flag.
type MCPListCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPResourceCapability is the allowlisted resource capability flags.
type MCPResourceCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPCapabilities is the typed allowlisted capability summary for a hosted server.
type MCPCapabilities struct {
	Tools       *MCPListCapability     `json:"tools,omitempty"`
	Prompts     *MCPListCapability     `json:"prompts,omitempty"`
	Resources   *MCPResourceCapability `json:"resources,omitempty"`
	Completions bool                   `json:"completions,omitempty"`
	Logging     bool                   `json:"logging,omitempty"`
}

// MCPCatalogInstance is one caller-visible hosted MCP server instance.
type MCPCatalogInstance struct {
	ID           string           `json:"id"`
	Handle       string           `json:"handle,omitempty"`
	Shared       bool             `json:"shared"`
	Mine         bool             `json:"mine"`
	Default      bool             `json:"default,omitempty"`
	State        string           `json:"state"`
	Reason       string           `json:"reason,omitempty"`
	ExitCode     *int             `json:"exitCode,omitempty"`
	Revision     int64            `json:"revision"`
	Generation   int64            `json:"generation"`
	Active       int              `json:"active"`
	Leases       int              `json:"leases"`
	ServerInfo   *MCPServerInfo   `json:"serverInfo,omitempty"`
	Capabilities *MCPCapabilities `json:"capabilities,omitempty"`
	LateReplies  uint64           `json:"lateReplies,omitempty"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

// MCPCatalogService is the safe, allowlisted hosted MCP server projection in
// the fleet catalog or show result (PLAN §6.6). Operational fields (argv, cwd,
// endpoint URL, secret/env names) never enter this payload.
type MCPCatalogService struct {
	HostID          string               `json:"hostId"`
	MachineName     string               `json:"machineName,omitempty"`
	Local           bool                 `json:"local,omitempty"`
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Revision        int64                `json:"revision"`
	State           string               `json:"state"`
	Freshness       string               `json:"freshness"`
	ObservedAt      *time.Time           `json:"observedAt,omitempty"`
	Transport       string               `json:"transport"`
	Lifecycle       string               `json:"lifecycle"`
	IdleSeconds     int64                `json:"idleSeconds,omitempty"`
	ReadySeconds    int64                `json:"readySeconds"`
	InstancePolicy  string               `json:"instancePolicy"`
	AccountMode     string               `json:"accountMode"`
	AccountLabel    string               `json:"accountLabel,omitempty"`
	Disabled        bool                 `json:"disabled,omitempty"`
	ProtocolVersion string               `json:"protocolVersion,omitempty"`
	Instances       []MCPCatalogInstance `json:"instances"`
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
	mcpHostFlagValue    = "HOST"
	mcpHandleFlagValue  = "HANDLE"
	mcpDurationValue    = "D"
	mcpNameValue        = "NAME"
	mcpModeValue        = "MODE"
	mcpTextValue        = "TEXT"
	mcpURLValue         = "URL"
	mcpNamePathValue    = "NAME=PATH"
	mcpHeaderPathValue  = "HEADER=PATH"
	mcpNameValuePair    = "NAME=VALUE"
	mcpCommandValue     = "COMMAND"
	mcpInstanceFlagName = "--instance"
	mcpIdentityFlagDesc = "act as this local application identity"
	mcpInstanceFlagDesc = "instance ID or mi1 handle"
	mcpHostServiceArg   = "[HOST/]NAME"
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
			{Name: hostFlag, Value: mcpHostFlagValue, Summary: "target host (defaults to local)"},
			{Name: cwdFlag, Value: pathFlagValue, Summary: "working directory on the target host for a stdio server or --run"},
			{Name: "--env", Value: mcpNameValuePair, Summary: "plain environment variable (repeatable)"},
			{Name: "--secret-env", Value: mcpNamePathValue, Summary: "secret file path read into NAME at launch (mode 0600)"},
			{Name: "--url", Value: mcpURLValue, Summary: "loopback Streamable HTTP endpoint of an HTTP server"},
			{Name: "--header-secret", Value: mcpHeaderPathValue, Summary: "credential header secret file sent to the HTTP server"},
			{Name: "--run", Value: mcpCommandValue, Summary: "command that starts the HTTP server at --url"},
			{Name: "--run-cwd", Value: pathFlagValue, Summary: "working directory for --run (alias for --cwd when --run is set)"},
			{Name: "--shared", Summary: "give every authorized caller one shared instance"},
			{Name: "--kept", Summary: "keep the server running instead of starting on demand (requires --shared)"},
			{Name: "--probe", Summary: "probe server/discover and tool schemas immediately after publishing"},
			{Name: "--idle", Value: mcpDurationValue, Summary: "idle timeout for an on-demand instance (default 15m)"},
			{Name: "--ready-timeout", Value: mcpDurationValue, Summary: "readiness probe timeout (default 30s)"},
			{Name: "--account-mode", Value: mcpModeValue, Summary: "none or host-account"},
			{Name: "--account-label", Value: mcpTextValue, Summary: "non-secret host account label shown to callers"},
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
			{Name: hostFlag, Value: mcpHostFlagValue, Summary: "limit listing to one machine name, exact host ID, or local"},
			{Name: identityFlag, Value: mcpNameValue, Summary: "scope instances and private catalog cache to this application identity"},
			{Name: timeoutFlag, Value: mcpDurationValue, Summary: "hard deadline for fleet queries (default 2s)"},
			{Name: cachedFlag, Summary: "read private cached catalog entries without dialing hosts"},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "List hosted MCP servers across saved hosts without waking hosts or starting services.",
		Example: "mesh mcp ls --json",
	},
	{
		Name:        "mcp.show",
		Command:     "mesh mcp show",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Network:     true,
		Flags: []Flag{
			{Name: identityFlag, Value: mcpNameValue, Summary: mcpIdentityFlagDesc},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Show one hosted MCP server definition and its instances.",
		Example: "mesh mcp show local/repo-tools --json",
	},
	{
		Name:        "mcp.start",
		Command:     "mesh mcp start",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: mcpInstanceFlagName, Value: mcpHandleFlagValue, Summary: mcpInstanceFlagDesc},
			{Name: identityFlag, Value: mcpNameValue, Summary: mcpIdentityFlagDesc},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Start a hosted MCP server instance and verify stateless 2026-07-28 readiness.",
		Example: "mesh mcp start local/repo-tools --json",
	},
	{
		Name:        "mcp.stop",
		Command:     "mesh mcp stop",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: mcpInstanceFlagName, Value: mcpHandleFlagValue, Summary: "instance ID or mi1 handle; omit to stop all managed instances"},
			{Name: identityFlag, Value: mcpNameValue, Summary: mcpIdentityFlagDesc},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Stop a hosted MCP server's running instances while keeping its definition.",
		Example: "mesh mcp stop local/repo-tools --json",
	},
	{
		Name:        "mcp.restart",
		Command:     "mesh mcp restart",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: mcpInstanceFlagName, Value: mcpHandleFlagValue, Summary: mcpInstanceFlagDesc},
			{Name: identityFlag, Value: mcpNameValue, Summary: mcpIdentityFlagDesc},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Replace a hosted MCP server instance with a fresh process generation.",
		Example: "mesh mcp restart local/repo-tools --json",
	},
	{
		Name:        "mcp.probe",
		Command:     "mesh mcp probe",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Network:     true,
		Flags: []Flag{
			{Name: mcpInstanceFlagName, Value: mcpHandleFlagValue, Summary: mcpInstanceFlagDesc},
			{Name: identityFlag, Value: mcpNameValue, Summary: mcpIdentityFlagDesc},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Probe a hosted MCP server's stateless 2026-07-28 readiness and tool schemas.",
		Example: "mesh mcp probe local/search --json",
	},
	{
		Name:        "mcp.enable",
		Command:     "mesh mcp enable",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Enable a disabled hosted MCP server definition.",
		Example: "mesh mcp enable local/search --json",
	},
	{
		Name:        "mcp.disable",
		Command:     "mesh mcp disable",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Disable a hosted MCP server definition and stop active instances.",
		Example: "mesh mcp disable local/search --json",
	},
	{
		Name:        "mcp.logs",
		Command:     "mesh mcp logs",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Network:     true,
		Flags: []Flag{
			{Name: mcpInstanceFlagName, Value: mcpHandleFlagValue, Summary: mcpInstanceFlagDesc},
			{Name: identityFlag, Value: mcpNameValue, Summary: mcpIdentityFlagDesc},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Read a hosted MCP server instance's bounded private stderr ring.",
		Example: "mesh mcp logs local/repo-tools --json",
	},
	{
		Name:        "mcp.remove",
		Command:     "mesh mcp rm",
		Args:        mcpHostServiceArg,
		Implemented: true,
		Mutates:     true,
		Network:     true,
		Flags: []Flag{
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Remove a hosted MCP server definition and stop its running instances.",
		Example: "mesh mcp rm local/repo-tools --json",
	},
}

func mcpServiceValueFlag(arg string) bool {
	switch arg {
	case "--env", "--secret-env", "--url", "--header-secret", "--run", "--run-cwd",
		"--idle", "--ready-timeout", "--account-mode", "--account-label":
		return true
	default:
		return false
	}
}
