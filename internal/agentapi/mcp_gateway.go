package agentapi

const (
	RouteUnknown         Code = "route.unknown"
	RouteIdentityChanged Code = "route.identity_changed"
)

func init() {
	Codes[RouteUnknown] = ErrorClass{Exit: 1}
	Codes[RouteIdentityChanged] = ErrorClass{Exit: 1}
	Operations = append(Operations, mcpGatewayOperations...)
}

func mcpGatewayValueFlag(arg string) bool {
	switch arg {
	case "--alias", "--native", "--include", "--exclude", "--profile":
		return true
	}
	return false
}

const (
	profileNameArg         = "NAME"
	casRevisionSummary     = "fail unless mcp-profiles.json is still at revision N"
	catalogIdentitySummary = "read host catalogs as this application identity"
)

var mcpGatewayOperations = []Operation{
	{
		Name: "mcp.profile.show", Command: "mesh mcp profile show", Args: "[NAME]", Implemented: true, Network: true,
		Flags: []Flag{
			{Name: identityFlag, Value: nameValue, Summary: catalogIdentitySummary},
			{Name: cachedFlag, Summary: "show the file only, without asking hosts for service states"},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Show gateway profiles and each selected service's state without starting services.",
		Example: "mesh mcp profile show dev --json",
	},
	{
		Name: "mcp.profile.add", Command: "mesh mcp profile add", Args: "NAME [HOST/SERVICE]", Implemented: true, Mutates: true, Network: true,
		Flags: []Flag{
			{Name: "--native", Value: "GROUPS", Summary: "all, none, or a comma list of native groups"},
			{Name: "--alias", Value: profileNameArg, Summary: "display alias for the selected service"},
			{Name: "--include", Value: "PATTERN", Summary: "expose only matching upstream names (repeatable)"},
			{Name: "--exclude", Value: "PATTERN", Summary: "hide matching upstream names (repeatable)"},
			{Name: mcpInstanceFlagName, Value: "HANDLE", Summary: mcpInstanceFlagDesc},
			{Name: identityFlag, Value: nameValue, Summary: catalogIdentitySummary},
			{Name: expectedRevisionFlag, Value: "N", Summary: casRevisionSummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Create a gateway profile or select one hosted MCP server in it by stable host and service ID.",
		Example: "mesh mcp profile add dev pc/repo-tools --alias repo --json",
	},
	{
		Name: "mcp.profile.remove", Command: "mesh mcp profile rm", Args: "NAME [ALIAS]", Implemented: true, Mutates: true,
		Flags:   []Flag{{Name: expectedRevisionFlag, Value: "N", Summary: casRevisionSummary}, {Name: jsonFlag, Summary: jsonFlagSummary}},
		Summary: "Remove a gateway profile, or one selected service from it.",
		Example: "mesh mcp profile rm dev repo --json",
	},
	{
		Name: "mcp.profile.alias", Command: "mesh mcp profile alias", Args: "NAME OLD NEW", Implemented: true, Mutates: true,
		Flags:   []Flag{{Name: expectedRevisionFlag, Value: "N", Summary: casRevisionSummary}, {Name: jsonFlag, Summary: jsonFlagSummary}},
		Summary: "Rename a selected service's display alias; exposed names do not change.",
		Example: "mesh mcp profile alias dev repo code --json",
	},
	{
		Name: "mcp.profile.repair", Command: "mesh mcp profile repair", Args: "NAME ALIAS", Implemented: true, Mutates: true, Network: true,
		Flags: []Flag{
			{Name: "--confirm", Summary: "rebind the entry to the service now published under its recorded name"},
			{Name: identityFlag, Value: nameValue, Summary: catalogIdentitySummary},
			{Name: expectedRevisionFlag, Value: "N", Summary: casRevisionSummary},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Find a removed and re-published service by its recorded name and rebind the entry with explicit confirmation.",
		Example: "mesh mcp profile repair dev repo --confirm --json",
	},
	{
		Name: "mcp.lease", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true,
		Summary: "Hold or release a hosted MCP server instance for a renewable TTL of at most 24 hours.", Example: "mesh_service_lease",
	},
}

// MCPProfileEntry is one hosted service selected by a gateway profile.
type MCPProfileEntry struct {
	Alias       string   `json:"alias"`
	HostID      string   `json:"hostId"`
	MachineName string   `json:"machineName,omitempty"`
	ServiceID   string   `json:"serviceId"`
	ServiceName string   `json:"serviceName,omitempty"`
	Instance    string   `json:"instance,omitempty"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
	State       string   `json:"state,omitempty"`
	Code        Code     `json:"code,omitempty"`
	Message     string   `json:"message,omitempty"`
	Revision    int64    `json:"revision,omitempty"`
}

// MCPProfile is one gateway profile. Native is null when every native group
// is selected.
type MCPProfile struct {
	Name     string            `json:"name"`
	Native   []string          `json:"native"`
	Services []MCPProfileEntry `json:"services"`
}

// MCPProfilesData is the data payload of mesh mcp profile commands.
type MCPProfilesData struct {
	Revision      uint64       `json:"revision"`
	Path          string       `json:"path"`
	Profiles      []MCPProfile `json:"profiles"`
	OverflowNames int          `json:"overflowNames"`
	Changed       bool         `json:"changed"`
}

// MCPProfileRepair proposes or records rebinding one entry by service name.
type MCPProfileRepair struct {
	Profile      string `json:"profile"`
	Alias        string `json:"alias"`
	HostID       string `json:"hostId"`
	ServiceName  string `json:"serviceName"`
	OldServiceID string `json:"oldServiceId"`
	NewServiceID string `json:"newServiceId"`
	Confirmed    bool   `json:"confirmed"`
	Revision     uint64 `json:"revision"`
}

// Gateway service states (PLAN section 7.3). They extend the hosted catalog
// states with what the gateway observed for one profile entry.
const (
	GatewayReady           = "ready"
	GatewayStopped         = "stopped"
	GatewayStale           = "stale"
	GatewayExpired         = "expired"
	GatewayOffline         = "offline"
	GatewayUnauthorized    = "unauthorized"
	GatewayUnsupported     = "unsupported"
	GatewayIncompatible    = "incompatible"
	GatewayFailed          = "failed"
	GatewayIdentityChanged = "identity_changed"
	GatewayNotFound        = "not_found"
)
