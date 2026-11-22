package mcphost

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

const instanceHandlePrefix = "mi1."

// CatalogServerInfo is the allowlisted upstream implementation identity shown
// in mcp.catalog and persisted in private client caches (PLAN §6.6).
type CatalogServerInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Title   string `json:"title,omitempty"`
}

// CatalogListCapability is the allowlisted list-change capability flag.
type CatalogListCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// CatalogResourceCapability is the allowlisted resource capability flags.
type CatalogResourceCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// CatalogCapabilities is the typed allowlisted capability summary of a ready
// hosted MCP server instance. Unrecognized upstream JSON keys are dropped.
type CatalogCapabilities struct {
	Tools       *CatalogListCapability     `json:"tools,omitempty"`
	Prompts     *CatalogListCapability     `json:"prompts,omitempty"`
	Resources   *CatalogResourceCapability `json:"resources,omitempty"`
	Completions bool                       `json:"completions,omitempty"`
	Logging     bool                       `json:"logging,omitempty"`
}

// CatalogInstance is the safe, allowlisted instance projection returned by
// mcp.catalog and stored in client catalog caches.
type CatalogInstance struct {
	ID           string               `json:"id"`
	ServiceID    string               `json:"serviceId,omitempty"`
	Shared       bool                 `json:"shared"`
	Mine         bool                 `json:"mine"`
	Default      bool                 `json:"default,omitempty"`
	State        string               `json:"state"`
	Reason       string               `json:"reason,omitempty"`
	ExitCode     *int                 `json:"exitCode,omitempty"`
	Revision     int64                `json:"revision"`
	Generation   int64                `json:"generation"`
	Active       int                  `json:"active"`
	Leases       int                  `json:"leases"`
	ServerInfo   *CatalogServerInfo   `json:"serverInfo,omitempty"`
	Capabilities *CatalogCapabilities `json:"capabilities,omitempty"`
	LateReplies  uint64               `json:"lateReplies,omitempty"`
	UpdatedAt    time.Time            `json:"updatedAt"`
}

// CatalogService is the safe, allowlisted service projection returned by
// mcp.catalog (PLAN §6.6). Operational configuration (argv/command, cwd,
// endpoint URL, env/secret/header names) never enters the catalog.
type CatalogService struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Revision        int64  `json:"revision"`
	Transport       string `json:"transport"`
	Lifecycle       string `json:"lifecycle"`
	IdleSeconds     int64  `json:"idleSeconds,omitempty"`
	ReadySeconds    int64  `json:"readySeconds"`
	InstancePolicy  string `json:"instancePolicy"`
	AccountMode     string `json:"accountMode"`
	AccountLabel    string `json:"accountLabel,omitempty"`
	Disabled        bool   `json:"disabled,omitempty"`
	ProtocolVersion string `json:"protocolVersion"`
	// ListingRevision changes whenever the host sees this service's
	// listings change (PLAN section 7.3).
	ListingRevision string            `json:"listingRevision,omitempty"`
	Instances       []CatalogInstance `json:"instances"`
}

// ProjectServerInfo extracts only the safe allowlisted serverInfo fields
// (name, version, title), dropping any unrecognized upstream keys.
func ProjectServerInfo(raw json.RawMessage) *CatalogServerInfo {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var parsed struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	if parsed.Name == "" && parsed.Version == "" && parsed.Title == "" {
		return nil
	}
	return &CatalogServerInfo{
		Name:    parsed.Name,
		Version: parsed.Version,
		Title:   parsed.Title,
	}
}

// ProjectCapabilities extracts only the typed standard MCP capability flags,
// dropping any unrecognized upstream fields or extensions.
func ProjectCapabilities(raw json.RawMessage) *CatalogCapabilities {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var parsed struct {
		Tools       *CatalogListCapability     `json:"tools"`
		Prompts     *CatalogListCapability     `json:"prompts"`
		Resources   *CatalogResourceCapability `json:"resources"`
		Completions json.RawMessage            `json:"completions"`
		Logging     json.RawMessage            `json:"logging"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	out := &CatalogCapabilities{
		Tools:       parsed.Tools,
		Prompts:     parsed.Prompts,
		Resources:   parsed.Resources,
		Completions: isJSONObject(parsed.Completions),
		Logging:     isJSONObject(parsed.Logging),
	}
	if out.Tools == nil && out.Prompts == nil && out.Resources == nil && !out.Completions && !out.Logging {
		return nil
	}
	return out
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

// Catalog returns the allowlisted, caller-scoped catalog projection for
// mcp.catalog (PLAN §6.6). Operational fields (command argv, cwd, endpoint
// URL, env/secret/header names) are excluded for every caller, including admin.
func (s *Supervisor) Catalog(caller Caller) []CatalogService {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CatalogService, 0, len(s.services))
	for _, d := range s.services {
		out = append(out, s.catalogLocked(caller, d))
	}
	slices.SortFunc(out, func(a, b CatalogService) int { return compareStrings(a.Name, b.Name) })
	return out
}

// CatalogService returns the safe catalog projection of one service.
func (s *Supervisor) CatalogService(caller Caller, ref string) (CatalogService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.serviceLocked(ref)
	if err != nil {
		return CatalogService{}, err
	}
	return s.catalogLocked(caller, d), nil
}

func (s *Supervisor) catalogLocked(caller Caller, d Definition) CatalogService {
	svc := CatalogService{
		ID:              d.ID,
		Name:            d.Name,
		Revision:        d.Revision,
		Transport:       d.Transport,
		Lifecycle:       d.Lifecycle,
		IdleSeconds:     int64(d.Idle / time.Second),
		ReadySeconds:    int64(d.ReadyTimeout / time.Second),
		InstancePolicy:  d.InstancePolicy,
		AccountMode:     d.AccountMode,
		AccountLabel:    d.AccountLabel,
		Disabled:        d.Disabled,
		ProtocolVersion: mcpwire.Version,
		ListingRevision: s.router.listings.token(d.ID, d.AccountLabel, d.Revision),
		Instances:       []CatalogInstance{},
	}
	for _, inst := range s.instances {
		if inst.rec.ServiceID != d.ID || !visible(caller, inst) {
			continue
		}
		svc.Instances = append(svc.Instances, s.catalogInstanceLocked(caller, inst))
	}
	slices.SortFunc(svc.Instances, func(a, b CatalogInstance) int { return compareStrings(a.ID, b.ID) })
	return svc
}

func (s *Supervisor) catalogInstanceLocked(caller Caller, inst *instance) CatalogInstance {
	r := inst.rec
	v := CatalogInstance{
		ID:         r.ID,
		ServiceID:  r.ServiceID,
		Shared:     r.Owner == "",
		Mine:       r.Owner != "" && caller.owns(r.Owner),
		Default:    r.Default,
		State:      r.State,
		Reason:     r.Reason,
		ExitCode:   r.ExitCode,
		Revision:   r.Revision,
		Generation: r.Generation,
		Leases:     len(r.Leases),
		UpdatedAt:  r.UpdatedAt,
	}
	if gen := inst.current; gen != nil {
		v.Active = gen.active
		v.ServerInfo = ProjectServerInfo(gen.serverInfo)
		v.Capabilities = ProjectCapabilities(gen.capabilities)
		if stdio, ok := gen.upstream.(*StdioUpstream); ok {
			v.LateReplies = stdio.LateReplies()
		}
	}
	return v
}

// resolveInstanceTarget validates an instance selector on the owning host.
// Bare instance IDs pass through; opaque mi1.<hostId>.<serviceId>.<instanceId>
// handles are verified against this host's ID and the resolved service's persisted ID.
func resolveInstanceTarget(hostID, serviceID, raw string) (string, error) {
	if !strings.HasPrefix(raw, instanceHandlePrefix) {
		return raw, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 || parts[0] != "mi1" {
		return "", fmt.Errorf("malformed instance handle %q: %w", raw, ErrInvalid)
	}
	handleHost, handleService, handleInstance := parts[1], parts[2], parts[3]
	if !validCanonicalHostID(handleHost) || !validHexID(handleService) || !validHexID(handleInstance) {
		return "", fmt.Errorf("malformed instance handle %q: %w", raw, ErrInvalid)
	}
	if hostID != "" && handleHost != hostID {
		return "", fmt.Errorf("instance handle belongs to host %s, not %s: %w", handleHost, hostID, ErrInvalid)
	}
	if handleService != serviceID {
		return "", fmt.Errorf("instance handle belongs to service %s, not %s: %w", handleService, serviceID, ErrNotFound)
	}
	return handleInstance, nil
}

func validCanonicalHostID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validHexID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
