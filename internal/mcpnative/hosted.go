package mcpnative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
)

const (
	maxLeaseSeconds = 24 * 60 * 60
	leaseOperation  = "mcp.lease"
)

// ServicesWithHosted is mesh_services_list data: serve routes plus the
// hosted MCP servers of the same hosts.
type ServicesWithHosted struct {
	agentapi.ServicesData
	HostedMCP []agentapi.MCPCatalogService `json:"hostedMcp"`
}

// HostedServiceDetail is mesh_service_inspect data for a hosted MCP server.
type HostedServiceDetail struct {
	HostedMCP agentapi.MCPCatalogService `json:"hostedMcp"`
}

// ServiceLeaseData is the result of mesh_service_lease.
type ServiceLeaseData struct {
	HostID   string                      `json:"hostId"`
	Service  string                      `json:"service"`
	LeaseID  string                      `json:"leaseId"`
	Expires  time.Time                   `json:"expires"`
	Job      string                      `json:"job,omitempty"`
	Released bool                        `json:"released"`
	Instance agentapi.MCPCatalogInstance `json:"instance"`
}

// LeaseServiceRequest holds or releases one hosted instance.
type LeaseServiceRequest struct {
	Host     string
	Service  string
	Instance string
	LeaseID  string
	Job      string
	TTL      time.Duration
	Timeout  time.Duration
}

// HostedServiceControls is the optional control family for hosted MCP
// instances. A Controls value without it reports the tool as unsupported.
type HostedServiceControls interface {
	LeaseService(ctx context.Context, req LeaseServiceRequest) (CallOutcome, error)
}

func hostedToolSpecs() []ToolSpec {
	return []ToolSpec{{
		Name:      "mesh_service_lease",
		Title:     "Hold a hosted MCP server instance running",
		Operation: leaseOperation,
		Group:     groupServices,
		Description: "Hold or release one hosted MCP server instance for a renewable TTL of at most 24 hours, so an on-demand instance stays running for upstream work Mesh cannot see. " +
			"lease_id is your key for the lease: repeating a call with the same key returns the same lease with the new TTL, and ttl_seconds 0 releases it. A lease naming a running native job of this caller ends with that job. Leases end when this application's device grant is retired.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","service","lease_id","ttl_seconds"],"properties":{` +
			`"host":{"type":"string","description":"Machine name, exact host ID, or local."},` +
			`"service":{"type":"string","description":"Hosted MCP server name or ID on that host."},` +
			`"instance":{"type":"string","description":"Instance ID or mi1 handle; omit for this caller's default or the shared instance."},` +
			`"lease_id":{"type":"string","pattern":"^[A-Za-z0-9._-]{1,64}$","description":"Caller-chosen lease key; the same key always names the same lease."},` +
			`"job":{"type":"string","description":"Optional running native job ID that bounds the lease."},` +
			`"ttl_seconds":{"type":"integer","minimum":0,"maximum":86400,"description":"Lease lifetime in seconds; 0 releases the lease."},` +
			`"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (default 5000)."}}}`),
		Annotations: idempotentWriteAnnotations(),
		Handler:     handleServiceLease,
	}}
}

func handleServiceLease(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host       string `json:"host"`
		Service    string `json:"service"`
		Instance   string `json:"instance"`
		LeaseID    string `json:"lease_id"`
		Job        string `json:"job"`
		TTLSeconds *int   `json:"ttl_seconds"`
		TimeoutMS  *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	service, err := requireNonEmpty(args.Service, "service")
	if err != nil {
		return CallOutcome{}, err
	}
	if !validLeaseKey(args.LeaseID) {
		return CallOutcome{}, invalidParam("lease_id must be 1 to 64 letters, digits, dots, underscores or hyphens")
	}
	if args.TTLSeconds == nil || *args.TTLSeconds < 0 || *args.TTLSeconds > maxLeaseSeconds {
		return CallOutcome{}, invalidParam("ttl_seconds must be between 0 and %d", maxLeaseSeconds)
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	hosted, ok := controls.(HostedServiceControls)
	if !ok {
		return CallOutcome{Envelope: errorEnvelope(leaseOperation, service, agentapi.ProtocolUnsupported, "This frontend cannot lease hosted MCP instances."), IsError: true}, nil
	}
	out, err := hosted.LeaseService(ctx, LeaseServiceRequest{Host: host, Service: service, Instance: strings.TrimSpace(args.Instance),
		LeaseID: strings.TrimSpace(args.LeaseID), Job: strings.TrimSpace(args.Job), TTL: time.Duration(*args.TTLSeconds) * time.Second, Timeout: timeout})
	return wrapControl("mesh_service_lease", out, err)
}

func validLeaseKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, r := range key {
		if !(r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func renderHostedData(b *strings.Builder, data any) {
	switch v := data.(type) {
	case ServicesWithHosted:
		renderServicesData(b, v.ServicesData)
		for _, s := range v.HostedMCP {
			b.WriteByte('\n')
			renderHostedService(b, s)
		}
	case HostedServiceDetail:
		renderHostedService(b, v.HostedMCP)
	case ServiceLeaseData:
		if v.Released {
			fmt.Fprintf(b, "released lease %s on %s/%s", v.LeaseID, v.HostID, escapeControlText(v.Service))
			return
		}
		fmt.Fprintf(b, "lease %s holds %s/%s instance %s until %s", v.LeaseID, v.HostID, escapeControlText(v.Service), v.Instance.ID, v.Expires.UTC().Format(time.RFC3339))
	}
}

func renderHostedService(b *strings.Builder, s agentapi.MCPCatalogService) {
	fmt.Fprintf(b, "%s/%s (hosted MCP, %s, %s, %s)", escapeControlText(s.MachineName), escapeControlText(s.Name), s.Transport, s.InstancePolicy, s.State)
}
