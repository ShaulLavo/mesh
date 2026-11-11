package update

import "time"

type HostTargetSubmission struct {
	Version          string `json:"version"`
	CurrentDigest    string `json:"currentDigest"`
	PolicyRevision   uint64 `json:"policyRevision"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	RetryEdge        *Edge  `json:"retryEdge,omitempty"`
}

type HostTargetRelease struct {
	Version  string `json:"version"`
	Metadata string `json:"metadata"`
	Manifest string `json:"manifest"`
	Binary   string `json:"binary"`
	Forward  string `json:"forward,omitempty"`
	Rollback string `json:"rollback,omitempty"`
}

// HostTargetPlan contains verified evidence only; it reserves no installation.
type HostTargetPlan struct {
	PolicyRevision uint64            `json:"policyRevision"`
	CurrentDigest  string            `json:"currentDigest"`
	Target         HostTargetRelease `json:"target"`
	Next           HostTargetRelease `json:"next"`
}

type HostTargetPhase string

const (
	HostTargetIdle       HostTargetPhase = "idle"
	HostTargetReserved   HostTargetPhase = "reserved"
	HostTargetInstalling HostTargetPhase = "installing"
	HostTargetWaiting    HostTargetPhase = "waiting"
	HostTargetCommitted  HostTargetPhase = "committed"
	HostTargetCancelled  HostTargetPhase = "cancelled"
	HostTargetAttention  HostTargetPhase = "attention"
)

// HostTargetStatus is a projection, never an acceptance credential. CurrentDigest
// is the initial approved source; RunningDigest follows verified committed hops.
type HostTargetStatus struct {
	Schema         int                `json:"schema"`
	Revision       uint64             `json:"revision"`
	PolicyRevision uint64             `json:"policyRevision"`
	Phase          HostTargetPhase    `json:"phase"`
	CurrentDigest  string             `json:"currentDigest,omitempty"`
	RunningDigest  string             `json:"runningDigest,omitempty"`
	Target         HostTargetRelease  `json:"target"`
	Next           *HostTargetRelease `json:"next,omitempty"`
	Request        string             `json:"request,omitempty"`
	Generation     uint64             `json:"generation,omitempty"`
	PendingSince   time.Time          `json:"pendingSince,omitempty"`
	Problem        *HostProblem       `json:"problem,omitempty"`
}
