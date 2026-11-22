package mcpnative

import (
	"encoding/json"
	"fmt"
)

const (
	groupTerminal  = "terminal"
	groupLifecycle = "lifecycle"
)

// terminalToolSpecs are the terminal job tools. They act only on terminal
// jobs this application identity created; a person attaching with mesh
// attach takes the terminal, and input and resize wait until they detach.
func terminalToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "mesh_terminal_open",
			Title:       "Open a keyed terminal job in an execution context",
			Operation:   "sessions.create",
			Group:       groupTerminal,
			Description: "Start a keyed terminal (PTY) job inside a durable execution context (requires mesh mcp --identity NAME), for programs that need a real terminal. Pass exactly one of command (shell string, refused for shell=none) or argv. Returns the opaque mj1 job handle at once; read the screen with mesh_terminal_screen, type with mesh_terminal_input, and read raw output with mesh_job_output stream=pty. Replaying request_key with the same intent returns the same job; after a lost reply use mesh_job_receipt. A person can take the terminal with mesh attach at any time; while they are attached, input and resize return terminal.attached.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["context","request_key"],"properties":{"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>)."},"request_key":{"type":"string","description":"Caller-chosen logical request key for idempotent dispatch and recovery via mesh_job_receipt."},"command":{"type":"string","description":"Shell command string (mutually exclusive with argv; refused when context shell is none)."},"argv":{"type":"array","items":{"type":"string"},"minItems":1,"description":"Direct executable and arguments (mutually exclusive with command)."},"cwd":{"type":"string","description":"Optional absolute or context-relative working directory for this job only."},"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional per-job environment overlay."},"cols":{"type":"integer","minimum":1,"maximum":2048,"description":"Initial terminal width (default 80)."},"rows":{"type":"integer","minimum":1,"maximum":2048,"description":"Initial terminal height (default 24)."},"term":{"type":"string","description":"TERM value for the program (default xterm-256color)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: openWorldDestructiveAnnotations(),
			Handler:     handleTerminalOpen,
		},
		{
			Name:        "mesh_terminal_input",
			Title:       "Type keyed input into a terminal job",
			Operation:   "sessions.input",
			Group:       groupTerminal,
			Description: "Write up to 1 MiB of keys to a terminal job created by this identity, at an expected decimal byte offset, or look up an earlier write_key acknowledgment when offset is omitted. Send control keys as bytes (for example \\r for Enter, \\u0003 for ctrl+c, \\u0004 for ctrl+d). Exact retries return the original acknowledgment; conflicting keys or stale offsets fail without writing. Returns terminal.attached without writing while a person is attached. Refused for pipe jobs; use mesh_job_input for pipe stdin.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job","write_key"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"write_key":{"type":"string","description":"Caller-chosen logical write key."},"offset":{"type":"string","description":"Expected nonnegative decimal byte offset; omit to look up write_key without writing."},"data":{"type":"string","description":"UTF-8 text bytes to type (mutually exclusive with data_base64)."},"data_base64":{"type":"string","description":"Base64-encoded bytes to type (mutually exclusive with data)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleTerminalInput,
		},
		{
			Name:        "mesh_terminal_resize",
			Title:       "Resize a terminal job",
			Operation:   "sessions.resize",
			Group:       groupTerminal,
			Description: "Set the size of a terminal job created by this identity. Returns terminal.attached without resizing while a person is attached, because their terminal decides the size.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job","cols","rows"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"cols":{"type":"integer","minimum":1,"maximum":2048,"description":"Terminal width."},"rows":{"type":"integer","minimum":1,"maximum":2048,"description":"Terminal height."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleTerminalResize,
		},
		{
			Name:        "mesh_terminal_screen",
			Title:       "Read a terminal job's rendered screen",
			Operation:   "sessions.screen",
			Group:       groupTerminal,
			Description: "Read the rendered screen of a terminal job created by this identity, as plain text lines at most 160 by 24, without resizing it or disturbing anyone attached. attached reports whether a person holds the terminal. After the job ends, returns the last saved screen when one exists.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job"],"properties":{"job":{"type":"string","description":"Opaque job handle (mj1.<hostId>.<sessionId>.<instance>)."},"context":{"type":"string","description":"Optional opaque context handle (mc1.<hostId>.<contextId>) to verify context ownership."},"cols":{"type":"integer","minimum":1,"maximum":160,"description":"Preview width (default 80)."},"rows":{"type":"integer","minimum":1,"maximum":24,"description":"Preview height (default 24)."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleTerminalScreen,
		},
	}
}

const serviceRouteSchema = `{"type":"object","additionalProperties":false,"required":["host","name"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID that owns the route."},"name":{"type":"string","description":"Route as mesh_services_list shows it, such as /dev or a service name."},"timeout_ms":{"type":"integer","minimum":100,"maximum":%d,"description":"Hard deadline in milliseconds."}}}`

// serviceToolSpecs change routes, list apps and run the non-interactive
// session lifecycle controls. Each host keeps its own restrictions: app owner
// operations run through the owning host's local socket, and hibernation
// accepts only detached agent sessions.
func serviceToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "mesh_service_start",
			Title:       "Start an on-demand route",
			Operation:   "services.start",
			Group:       groupServices,
			Description: "Start a route that has a run command now and wait until it is ready (default 30 s, up to 120 s). This also ends the stop of a keep-running route.",
			InputSchema: routeSchema(maxServiceMS),
			Annotations: destructiveAnnotations(true),
			Handler:     handleServiceStart,
		},
		{
			Name:        "mesh_service_stop",
			Title:       "Stop an on-demand route",
			Operation:   "services.stop",
			Group:       groupServices,
			Description: "Stop a route's run command now. Its listeners stay bound. A route with an idle window starts again on its next connection through Mesh; a keep-running route stays stopped until mesh_service_start, a change of its definition, or a daemon restart.",
			InputSchema: routeSchema(maxTimeoutMS),
			Annotations: destructiveAnnotations(true),
			Handler:     handleServiceStop,
		},
		{
			Name:        "mesh_service_publish",
			Title:       "Publish or replace a private route",
			Operation:   "services.publish",
			Group:       groupServices,
			Description: "Publish one private, tailnet-only route on a host, the same as mesh serve HOST TARGET. target is a port, a URL, or a directory on that host. With run, the host starts the command on the first connection through Mesh and stops it when idle (idle_ms, default 15 minutes), or keeps it running with keep_running. run on another machine needs an absolute cwd on that machine. Replacing a route keeps its private hostname and recipe lifetime unless given. The host previews the definition first and publishes exactly what it previewed.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","target"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"target":{"type":"string","description":"Port, URL or directory on the host."},"route":{"type":"string","description":"Route path such as /blog (default from target)."},"label":{"type":"string","description":"Display name shown in service lists."},"run":{"type":"string","description":"Command that serves the port; needs a port target."},"cwd":{"type":"string","description":"Directory run starts in, absolute on the host."},"env":{"type":"array","maxItems":32,"items":{"type":"string"},"description":"KEY=VALUE entries added to the environment of run."},"listen":{"type":"array","maxItems":32,"items":{"type":"string"},"description":"PUBLIC=UPSTREAM local port proxies on the host."},"keep_running":{"type":"boolean","description":"Start run now, never stop it for idleness, and restart it after it exits."},"idle_ms":{"type":"integer","minimum":1000,"description":"Stop run after this long with no connection through Mesh."},"ready_timeout_ms":{"type":"integer","minimum":1000,"description":"How long a starting run holds connections before failing them (default 60000)."},"files":{"type":"boolean","description":"Enable directory listings for a directory target."},"isolate":{"type":"boolean","description":"Send cross-origin isolation headers."},"timeout_ms":{"type":"integer","minimum":100,"maximum":120000,"description":"Hard deadline in milliseconds (default 30000)."}}}`),
			Annotations: openWorldDestructiveAnnotations(),
			Handler:     handleServicePublish,
		},
		{
			Name:        "mesh_service_unpublish",
			Title:       "Remove a private route",
			Operation:   "services.unpublish",
			Group:       groupServices,
			Description: "Remove one route from its host, the same as mesh unserve, stopping its run command if it has one. Returns service.not_found only when the host's own route list shows the route absent; unlike mesh unserve --host, which succeeds then. A retry after a lost reply therefore reports service.not_found once the first attempt removed the route. Any other error leaves the route's state unknown; read it with mesh_service_inspect before retrying.",
			InputSchema: routeSchema(maxTimeoutMS),
			Annotations: destructiveAnnotations(true),
			Handler:     handleServiceUnpublish,
		},
		{
			Name:        "mesh_apps_list",
			Title:       "List a host's temporary apps",
			Operation:   "apps.list",
			Group:       groupServices,
			Description: "List the temporary apps a host owns (at most 200), with state, expiry and URL. App owner operations run through the owning host's local socket, reached over Mesh SSH for another machine, the same as mesh app list.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host"],"properties":{"host":{"type":"string","description":"Machine name, exact host ID, or local for this machine."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleAppsList,
		},
		{
			Name:        "mesh_session_hibernate",
			Title:       "Hibernate a detached agent session",
			Operation:   "sessions.hibernate",
			Group:       groupLifecycle,
			Description: "Stop a detached Claude or Codex session to free its memory while its conversation stays resumable; the next mesh attach resumes it. The host refuses plain shells and attached sessions. Never attaches.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","session_id"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"session_id":{"type":"string","description":"Canonical Mesh session ID."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (default 12000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleSessionHibernate,
		},
		{
			Name:        "mesh_session_recover",
			Title:       "Start a detached replacement for an interrupted session",
			Operation:   "sessions.recover",
			Group:       groupLifecycle,
			Description: "Start a replacement for an interrupted session (after a reboot) that keeps its previous output, without attaching to it: action command restarts the saved command, shell opens a shell in the saved directory, and agent resumes the saved agent conversation. Returns the new session ID; a person attaches with mesh attach. Application-owned jobs and pipe workers are never recovered this way.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","session_id","action"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID."},"session_id":{"type":"string","description":"Canonical ID of the interrupted session."},"action":{"type":"string","enum":["command","shell","agent"],"description":"What the replacement runs."},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (default 12000)."}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleSessionRecover,
		},
	}
}

func routeSchema(maxMS int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(serviceRouteSchema, maxMS))
}
