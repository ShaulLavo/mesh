package mcpnative

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/shaul/mesh/internal/mcpwire"
)

// profileTerminal names both the profile group and the registry group of
// the terminal tools.
const profileTerminal = "terminal"

// profileGroups maps the gateway profile's native groups to registry groups.
// mesh_skill is always present.
var profileGroups = map[string][]string{
	"hosts":         {groupHosts},
	"contexts":      {groupContexts},
	"exec":          {groupExec, groupJobs},
	"sessions":      {groupSessions},
	"services":      {groupServices, "lifecycle"},
	profileTerminal: {profileTerminal},
	"files":         {groupFiles},
	"fleet":         {groupFleet, groupProfile},
}

type gatewayTool struct {
	name  string
	group string
	raw   json.RawMessage
}

// GatewayNative serves the native tools a gateway profile selects. It shares
// the dispatcher, envelopes and progress reporting of the plain native
// frontend, and filters by the profile's groups on every request so a
// profile edit takes effect without a restart.
type GatewayNative struct {
	core  *dispatcher
	tools []gatewayTool
}

// NewGatewayNative builds the gateway's native tool source.
func NewGatewayNative(opts ServerOptions) (*GatewayNative, error) {
	core, err := newDispatcher(opts)
	if err != nil {
		return nil, err
	}
	n := &GatewayNative{core: core}
	for _, def := range core.registry.Definitions() {
		spec, _ := core.registry.Lookup(def.Name)
		raw, err := json.Marshal(def)
		if err != nil {
			return nil, fmt.Errorf("encode native tool %s: %w", def.Name, err)
		}
		n.tools = append(n.tools, gatewayTool{name: def.Name, group: spec.Group, raw: raw})
	}
	return n, nil
}

// selected reports whether a registry group is in the profile's groups. A
// nil group list selects every native tool.
func selected(groups []string, group string) bool {
	if groups == nil || group == groupSkill {
		return true
	}
	for name, registryGroups := range profileGroups {
		if slices.Contains(groups, name) && slices.Contains(registryGroups, group) {
			return true
		}
	}
	return false
}

// Tools returns the selected definitions in registry order.
func (n *GatewayNative) Tools(groups []string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(n.tools))
	for _, t := range n.tools {
		if selected(groups, t.group) {
			out = append(out, t.raw)
		}
	}
	return out
}

// Has reports whether name is a selected native tool.
func (n *GatewayNative) Has(groups []string, name string) bool {
	for _, t := range n.tools {
		if t.name == name {
			return selected(groups, t.group)
		}
	}
	return false
}

// Call runs one native tools/call and emits its progress and terminal reply.
func (n *GatewayNative) Call(ctx context.Context, req mcpwire.Message, emit func(raw []byte, terminal bool) error) error {
	var notify func(ProgressUpdate)
	if token := extractProgressToken(req); len(token) > 0 {
		notify = func(update ProgressUpdate) {
			if raw, ok := marshalProgressNotification(token, update); ok {
				_ = emit(raw, false)
			}
		}
	}
	reply, fault := n.core.handleCallTool(ctx, req, notify)
	if fault != nil {
		return emit(encodeBoundedFault(fault, mcpwire.MaxMessage), true)
	}
	return emit(reply, true)
}
