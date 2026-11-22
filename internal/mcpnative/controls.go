package mcpnative

import (
	"context"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
)

// MetaPrefix is the reverse-DNS prefix for Mesh-owned MCP _meta keys.
const MetaPrefix = "com.github.shaullavo.mesh/"

// MetaTargetKey identifies the execution target on native MCP tool results.
const MetaTargetKey = MetaPrefix + "target"

// TargetMeta carries non-secret target location metadata in results and _meta.
type TargetMeta struct {
	HostID    string `json:"hostId"`
	HostName  string `json:"hostName,omitempty"`
	Account   string `json:"account,omitempty"`
	ContextID string `json:"contextId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// CallOutcome is returned by Controls implementations and wrapped into an MCP
// tool result by internal/mcpnative.
type CallOutcome struct {
	Envelope agentapi.Envelope
	Target   *TargetMeta
	IsError  bool
}

// ProgressUpdate reports in-flight progress while a tool call waits.
type ProgressUpdate struct {
	Progress float64
	Total    *float64
	Message  string
}

// Controls is the transport-independent boundary between native MCP tool
// handlers and Mesh's host/daemon control layer.
type Controls interface {
	ListHosts(ctx context.Context, req ListHostsRequest) (CallOutcome, error)
	InspectHost(ctx context.Context, req InspectHostRequest) (CallOutcome, error)
	WakeHost(ctx context.Context, req WakeHostRequest) (CallOutcome, error)
	ListSessions(ctx context.Context, req ListSessionsRequest) (CallOutcome, error)
	InspectSession(ctx context.Context, req InspectSessionRequest) (CallOutcome, error)
	ListServices(ctx context.Context, req ListServicesRequest) (CallOutcome, error)
	InspectService(ctx context.Context, req InspectServiceRequest) (CallOutcome, error)
	OpenContext(ctx context.Context, req OpenContextRequest) (CallOutcome, error)
	GetContext(ctx context.Context, req ContextRefRequest) (CallOutcome, error)
	ListContexts(ctx context.Context, req ListContextsRequest) (CallOutcome, error)
	ContextReceipt(ctx context.Context, req ContextReceiptRequest) (CallOutcome, error)
	UpdateContext(ctx context.Context, req UpdateContextRequest) (CallOutcome, error)
	CloseContext(ctx context.Context, req CloseContextRequest) (CallOutcome, error)
	Exec(ctx context.Context, req ExecRequest, notify func(ProgressUpdate)) (CallOutcome, error)
	JobStatus(ctx context.Context, req JobRefRequest) (CallOutcome, error)
	JobWait(ctx context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error)
	JobOutput(ctx context.Context, req JobOutputRequest) (CallOutcome, error)
	JobReceipt(ctx context.Context, req JobReceiptRequest) (CallOutcome, error)
	ListJobs(ctx context.Context, req ListJobsRequest) (CallOutcome, error)
	JobInput(ctx context.Context, req JobInputRequest) (CallOutcome, error)
	JobSignal(ctx context.Context, req JobSignalRequest) (CallOutcome, error)
	JobCancel(ctx context.Context, req JobRefRequest) (CallOutcome, error)
	JobRemove(ctx context.Context, req JobRefRequest) (CallOutcome, error)
	TerminalControls
	ServiceControls
}

type ListHostsRequest struct {
	Cached  bool
	Timeout time.Duration
}

type InspectHostRequest struct {
	Host    string
	Cached  bool
	Timeout time.Duration
}

type WakeHostRequest struct {
	Host    string
	Timeout time.Duration
}

// WakeResult is the structured data returned by mesh_host_wake.
type WakeResult struct {
	HostID        string `json:"hostId"`
	MachineName   string `json:"machineName,omitempty"`
	AlreadyOnline bool   `json:"alreadyOnline"`
	Sender        string `json:"sender,omitempty"`
}

type ListSessionsRequest struct {
	Host    string
	All     bool
	Detail  bool
	Limit   int
	Page    string
	Timeout time.Duration
}

type InspectSessionRequest struct {
	Host      string
	SessionID string
	Preview   bool
	Timeout   time.Duration
}

type ListServicesRequest struct {
	Host    string
	Timeout time.Duration
}

type InspectServiceRequest struct {
	Host    string
	Name    string
	Timeout time.Duration
}

// ServiceDetail is the structured data returned by mesh_service_inspect.
type ServiceDetail struct {
	Service agentapi.ServiceRow `json:"service"`
}

type OpenContextRequest struct {
	Host       string
	RequestKey string
	Cwd        string
	Shell      string
	Env        map[string]string
	Label      string
	Timeout    time.Duration
}

type ContextRefRequest struct {
	Context ContextRef
	Timeout time.Duration
}

type ListContextsRequest struct {
	Host    string
	Timeout time.Duration
}

type ContextReceiptRequest struct {
	Host       string
	RequestKey string
	Timeout    time.Duration
}

type UpdateContextRequest struct {
	Context          ContextRef
	ExpectedRevision uint64
	Cwd              string
	SetEnv           map[string]string
	UnsetEnv         []string
	Label            *string
	Timeout          time.Duration
}

type CloseContextRequest struct {
	Context          ContextRef
	ExpectedRevision *uint64
	Timeout          time.Duration
}

type ExecRequest struct {
	Context     ContextRef
	RequestKey  string
	Command     string
	Argv        []string
	Cwd         string
	Env         map[string]string
	Stdin       string
	WaitMS      int
	OutputBytes int
}

type JobRefRequest struct {
	Job     JobRef
	Context *ContextRef
	Timeout time.Duration
}

type JobWaitRequest struct {
	Job     JobRef
	Context *ContextRef
	WaitMS  int
}

type JobOutputRequest struct {
	Job      JobRef
	Context  *ContextRef
	Stream   string
	Cursor   string
	MaxBytes int
	Timeout  time.Duration
}

type JobReceiptRequest struct {
	Host       string
	Context    *ContextRef
	RequestKey string
	Timeout    time.Duration
}

type ListJobsRequest struct {
	Host    string
	Context *ContextRef
	All     bool
	Limit   int
	Page    string
	Timeout time.Duration
}

type JobInputRequest struct {
	Job      JobRef
	Context  *ContextRef
	WriteKey string
	Offset   *uint64
	Data     []byte
	EOF      bool
	// Kind is the job kind the tool writes to: pipe stdin or terminal keys.
	// The host refuses the other kind, so the two never mix.
	Kind    string
	Timeout time.Duration
}

type JobSignalRequest struct {
	Job     JobRef
	Context *ContextRef
	Signal  string
	Timeout time.Duration
}

// ExecStreamGap records a cursor/retention gap on one output stream.
type ExecStreamGap struct {
	Reason         string `json:"reason,omitempty"`
	EarliestCursor string `json:"earliestCursor,omitempty"`
}

// ExecStreamData is the bounded initial stream snippet returned by mesh_exec.
type ExecStreamData struct {
	Encoding   string         `json:"encoding"`
	Data       string         `json:"data"`
	Start      string         `json:"start"`
	End        string         `json:"end"`
	NextCursor string         `json:"nextCursor"`
	EOF        bool           `json:"eof"`
	Gap        *ExecStreamGap `json:"gap"`
}

// ExecResultData is the structured result returned by mesh_exec.
type ExecResultData struct {
	Job        string                     `json:"job"`
	RequestKey string                     `json:"requestKey"`
	Target     TargetMeta                 `json:"target"`
	State      string                     `json:"state"`
	Wait       string                     `json:"wait,omitempty"`
	Result     *agentapi.ExecutionResult  `json:"result,omitempty"`
	Receipt    *agentapi.ExecutionReceipt `json:"receipt,omitempty"`
	Stdout     *ExecStreamData            `json:"stdout,omitempty"`
	Stderr     *ExecStreamData            `json:"stderr,omitempty"`
}

// JobStateData is the structured data returned by job status, wait, receipt,
// signal, cancel, and remove tools. It preserves the full agentapi receipt
// alongside the opaque job handle and target metadata.
type JobStateData struct {
	Job        string                     `json:"job"`
	RequestKey string                     `json:"requestKey,omitempty"`
	Target     TargetMeta                 `json:"target"`
	State      string                     `json:"state"`
	Wait       string                     `json:"wait,omitempty"`
	Delivery   string                     `json:"delivery,omitempty"`
	Result     *agentapi.ExecutionResult  `json:"result,omitempty"`
	Receipt    *agentapi.ExecutionReceipt `json:"receipt"`
}

// JobOutputData is the structured data returned by mesh_job_output.
type JobOutputData struct {
	Job            string     `json:"job"`
	Target         TargetMeta `json:"target"`
	Stream         string     `json:"stream"`
	Generation     string     `json:"generation"`
	Encoding       string     `json:"encoding"`
	Data           string     `json:"data"`
	Start          string     `json:"start,omitempty"`
	End            string     `json:"end,omitempty"`
	StartCursor    string     `json:"startCursor"`
	NextCursor     string     `json:"nextCursor"`
	EarliestCursor string     `json:"earliestCursor"`
	EndCursor      string     `json:"endCursor"`
	ObservedEnd    string     `json:"observedEnd"`
	StreamClosed   bool       `json:"streamClosed"`
	EOF            bool       `json:"eof"`
	CloseReason    string     `json:"closeReason"`
	Loss           string     `json:"loss"`
	Available      bool       `json:"available"`
	Gap            bool       `json:"gap"`
	Reason         string     `json:"reason,omitempty"`
}

// JobListItem describes one keyed execution job in mesh_jobs_list.
type JobListItem struct {
	Job       string     `json:"job"`
	HostID    string     `json:"hostId"`
	SessionID string     `json:"sessionId"`
	Instance  string     `json:"instance"`
	ContextID string     `json:"contextId,omitempty"`
	State     string     `json:"state"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Command   []string   `json:"command,omitempty"`
	Cwd       string     `json:"cwd,omitempty"`
}

// JobListData is the structured data returned by mesh_jobs_list.
type JobListData struct {
	HostID    string             `json:"hostId"`
	Freshness agentapi.Freshness `json:"freshness"`
	Jobs      []JobListItem      `json:"jobs"`
	NextPage  *string            `json:"nextPage"`
}

// JobInputData is the structured data returned by mesh_job_input.
type JobInputData struct {
	Job      string     `json:"job"`
	Target   TargetMeta `json:"target"`
	WriteKey string     `json:"writeKey"`
	Offset   string     `json:"offset"`
	Length   int        `json:"length"`
	Closed   bool       `json:"closed"`
}

// SkillData is the structured data returned by mesh_skill.
type SkillData struct {
	File    string   `json:"file"`
	Digest  string   `json:"digest"`
	Files   []string `json:"files"`
	Content string   `json:"content"`
}
