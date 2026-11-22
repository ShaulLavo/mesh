package agentapi

import "time"

// Terminal job and service control results shared by the CLI and mesh mcp.

const (
	TerminalAttached Code = "terminal.attached"
	TerminalClosed   Code = "terminal.closed"
)

func init() {
	Codes[TerminalAttached] = ErrorClass{Exit: 1}
	Codes[TerminalClosed] = ErrorClass{Exit: 1}
}

// TerminalSize is a terminal job's size after a resize.
type TerminalSize struct {
	HostID    string `json:"hostId"`
	SessionID string `json:"sessionId"`
	Instance  string `json:"instance"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// TerminalScreen is the rendered screen of a terminal job, read without
// changing its size or taking it from an attached person.
type TerminalScreen struct {
	HostID     string     `json:"hostId"`
	SessionID  string     `json:"sessionId"`
	Instance   string     `json:"instance"`
	ObservedAt *time.Time `json:"observedAt"`
	// Attached reports a person attached right now. While true, terminal
	// input and resize return terminal.attached.
	Attached bool     `json:"attached"`
	Title    string   `json:"title,omitempty"`
	Lines    []string `json:"lines"`
	Cols     int      `json:"cols"`
	Rows     int      `json:"rows"`
	Live     bool     `json:"live"`
}

// ServiceChange is a route after a start, stop or publish.
type ServiceChange struct {
	Service ServiceRow `json:"service"`
	// State is the route's demand state, such as "running" or "stopped".
	State     string   `json:"state"`
	SessionID string   `json:"sessionId,omitempty"`
	Warnings  []string `json:"warnings"`
}

// ServiceRemoval confirms that one route no longer exists on its host.
type ServiceRemoval struct {
	HostID  string `json:"hostId"`
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// AppRow is one temporary app owned by a host.
type AppRow struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Cleanup   string    `json:"cleanup"`
	Ready     bool      `json:"ready"`
	ExpiresAt time.Time `json:"expiresAt"`
	URL       string    `json:"url"`
}

// AppsData lists a host's temporary apps, at most MaxAppRows of them.
type AppsData struct {
	HostID    string   `json:"hostId"`
	Apps      []AppRow `json:"apps"`
	Truncated bool     `json:"truncated"`
}

// MaxAppRows bounds one app listing.
const MaxAppRows = 200

// SessionHibernation confirms that a person's detached agent session was
// stopped with its conversation kept resumable.
type SessionHibernation struct {
	HostID     string `json:"hostId"`
	SessionID  string `json:"sessionId"`
	Hibernated bool   `json:"hibernated"`
}

// SessionRecovery names the session that replaced an interrupted one. The
// replacement starts detached; nothing attaches to it.
type SessionRecovery struct {
	HostID        string `json:"hostId"`
	SessionID     string `json:"sessionId"`
	RecoveredFrom string `json:"recoveredFrom"`
	Action        string `json:"action"`
	AgentStatus   string `json:"agentStatus,omitempty"`
}

// terminalOperations are the terminal job controls with a CLI command, and
// the service controls only mesh mcp offers.
var terminalOperations = []Operation{
	{Name: "sessions.resize", Command: "mesh session resize", Args: sessionIDValue, Implemented: true, Network: true, Mutates: true, Flags: append(executionControlFlags("resize"), Flag{Name: colsFlag, Value: "N", Required: true, Summary: "terminal width"}, Flag{Name: rowsFlag, Value: "N", Required: true, Summary: "terminal height"}), Summary: "Resize a keyed terminal job while no person is attached.", Example: "mesh session resize --host HOST_ID 7K3D --instance INSTANCE --cols 120 --rows 40 --json"},
	{Name: "sessions.screen", Command: "mesh session screen", Args: sessionIDValue, Implemented: true, Network: true, Flags: append(executionControlFlags("screen"), Flag{Name: colsFlag, Value: "N", Summary: "preview width up to 160"}, Flag{Name: rowsFlag, Value: "N", Summary: "preview height up to 24"}), Summary: "Read a keyed terminal job's rendered screen without resizing it.", Example: "mesh session screen --host HOST_ID 7K3D --instance INSTANCE --json"},
	{Name: "services.start", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true, Summary: "Start an on-demand route now and wait until it is ready.", Example: "mesh_service_start"},
	{Name: "services.stop", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true, Summary: "Stop an on-demand route now.", Example: "mesh_service_stop"},
	{Name: "services.publish", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true, Summary: "Publish or replace one private route on a host.", Example: "mesh_service_publish"},
	{Name: "services.unpublish", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true, Summary: "Remove one private route from a host.", Example: "mesh_service_unpublish"},
	{Name: "apps.list", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Summary: "List the temporary apps a host owns.", Example: "mesh_apps_list"},
	{Name: "sessions.hibernate", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true, Summary: "Hibernate a detached agent session so it resumes on the next attach.", Example: "mesh_session_hibernate"},
	{Name: "sessions.recover", Command: mcpSurfaceCommand, Implemented: true, MCPOnly: true, Network: true, Mutates: true, Summary: "Start a detached replacement for an interrupted session.", Example: "mesh_session_recover"},
}
