package agentapi

import "time"

type ProfileState string
type Freshness string
type Completeness string

const (
	FreshnessLive       Freshness    = "live"
	FreshnessCached     Freshness    = "cached"
	CompletenessSummary Completeness = "summary"
	CompletenessFull    Completeness = "full"
)

type Profile struct {
	Description       string            `json:"description"`
	Labels            map[string]string `json:"labels"`
	Warnings          []string          `json:"warnings"`
	Instructions      *string           `json:"instructions"`
	InstructionsBytes int               `json:"instructionsBytes"`
}

type ProfileSection struct {
	State        ProfileState `json:"state"`
	Problem      string       `json:"problem,omitempty"`
	Revision     *uint64      `json:"revision"`
	Digest       string       `json:"digest,omitempty"`
	UpdatedAt    *time.Time   `json:"updatedAt,omitempty"`
	Freshness    Freshness    `json:"freshness"`
	Completeness Completeness `json:"completeness"`
	ReceivedAt   *time.Time   `json:"receivedAt,omitempty"`
	Profile      *Profile     `json:"profile,omitempty"`
}

type FleetSection struct {
	ProfileSection
	Fleet  string `json:"fleet,omitempty"`
	Author string `json:"author,omitempty"`
}

type Declared struct {
	Host  ProfileSection `json:"host"`
	Fleet FleetSection   `json:"fleet"`
}

type HostsData struct {
	ReceivedAt time.Time     `json:"receivedAt"`
	Fleet      FleetSection  `json:"fleet"`
	Hosts      []HostSummary `json:"hosts"`
}

type HostSummary struct {
	HostID       *string   `json:"hostId"`
	MachineName  string    `json:"machineName,omitempty"`
	Local        bool      `json:"local"`
	NameShadowed bool      `json:"nameShadowed"`
	Reachability string    `json:"reachability"`
	Observed     *Observed `json:"observed"`
	Declared     Declared  `json:"declared"`
}

type Observed struct {
	Version    string    `json:"version"`
	Platform   string    `json:"platform"`
	Daemon     string    `json:"daemon"`
	ObservedAt time.Time `json:"observedAt"`
	Freshness  Freshness `json:"freshness"`
}

type Access struct {
	Reason            string     `json:"reason"`
	State             string     `json:"state"`
	ObservedAt        *time.Time `json:"observedAt"`
	Transport         string     `json:"transport"`
	Caller            *string    `json:"caller"`
	Grant             string     `json:"grant"`
	Enforcement       string     `json:"enforcement"`
	Principal         *string    `json:"principal"`
	ScopedPermissions *[]string  `json:"scopedPermissions"`
	Summary           string     `json:"summary"`
}

type HostDetail struct {
	Host   HostSummary `json:"host"`
	Access Access      `json:"access"`
}

type SessionSummary struct {
	ID              string     `json:"id"`
	HostID          string     `json:"hostId"`
	Kind            string     `json:"kind"`
	State           string     `json:"state"`
	Hibernated      bool       `json:"hibernated"`
	RecoveryPending bool       `json:"recoveryPending"`
	Label           string     `json:"label,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	LastAttachedAt  *time.Time `json:"lastAttachedAt,omitempty"`
	DetachedAt      *time.Time `json:"detachedAt,omitempty"`
	ExitCode        *int       `json:"exitCode,omitempty"`
	Command         []string   `json:"command,omitempty"`
	Cwd             string     `json:"cwd,omitempty"`
}

type SessionPage struct {
	HostID    string           `json:"hostId"`
	Freshness Freshness        `json:"freshness"`
	Sessions  []SessionSummary `json:"sessions"`
	NextPage  *string          `json:"nextPage"`
}

type LiveInspection struct {
	ObservedAt        time.Time  `json:"observedAt"`
	Attached          bool       `json:"attached"`
	CurrentDirectory  string     `json:"currentDirectory,omitempty"`
	ForegroundCommand string     `json:"foregroundCommand,omitempty"`
	TerminalTitle     string     `json:"terminalTitle,omitempty"`
	LastOutputAt      *time.Time `json:"lastOutputAt,omitempty"`
	Preview           []string   `json:"preview,omitempty"`
}

type SavedInspection struct {
	CheckpointAt    *time.Time `json:"checkpointAt"`
	Directory       string     `json:"directory"`
	DirectorySource string     `json:"directorySource"`
	Command         []string   `json:"command"`
	TerminalTitle   string     `json:"terminalTitle"`
	LastOutputAt    *time.Time `json:"lastOutputAt"`
	Preview         []string   `json:"preview"`
}

type SessionDetail struct {
	Session   SessionSummary   `json:"session"`
	Freshness Freshness        `json:"freshness"`
	LiveState string           `json:"liveState"`
	Live      *LiveInspection  `json:"live"`
	Saved     *SavedInspection `json:"saved"`
}

type ServiceRow struct {
	HostID      string     `json:"hostId"`
	MachineName string     `json:"machineName"`
	Name        string     `json:"name"`
	DisplayName string     `json:"displayName"`
	Kind        string     `json:"kind"`
	Scope       string     `json:"scope"`
	URL         *string    `json:"url"`
	Health      string     `json:"health"`
	Problem     string     `json:"problem"`
	Freshness   Freshness  `json:"freshness"`
	ObservedAt  *time.Time `json:"observedAt"`
}

type ServicesData struct {
	Services   []ServiceRow `json:"services"`
	HostErrors []Error      `json:"hostErrors"`
}

type ProfileWrite struct {
	Revision    uint64     `json:"revision"`
	Digest      string     `json:"digest"`
	Changed     bool       `json:"changed"`
	Committed   bool       `json:"committed"`
	Publication string     `json:"publication"`
	Queued      bool       `json:"queued"`
	FleetSaved  bool       `json:"fleetSaved"`
	Members     []Delivery `json:"members"`
}

type FleetStatus struct {
	Replica    FleetSection `json:"replica"`
	FleetSaved bool         `json:"fleetSaved"`
	Fleet      string       `json:"fleet"`
	Members    []Delivery   `json:"members"`
}

type DeliveryError struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Delivery struct {
	HostID         string         `json:"hostId"`
	MachineName    string         `json:"machineName,omitempty"`
	State          string         `json:"state"`
	QueuedAt       time.Time      `json:"queuedAt"`
	SettledAt      *time.Time     `json:"settledAt,omitempty"`
	LastError      *DeliveryError `json:"lastError,omitempty"`
	MemberRevision uint64         `json:"memberRevision,omitempty"`
	MemberDigest   string         `json:"memberDigest,omitempty"`
}

type SkillRoot struct {
	Path              string   `json:"path"`
	State             string   `json:"state"`
	Registered        bool     `json:"registered"`
	RegistrationState string   `json:"registrationState"`
	MeshVersion       *string  `json:"meshVersion"`
	Bundle            *string  `json:"bundle"`
	ModifiedFiles     []string `json:"modifiedFiles"`
	MissingFiles      []string `json:"missingFiles"`
	ProposedFiles     []string `json:"proposedFiles"`
	RetainedFiles     []string `json:"retainedFiles"`
	Problems          []Error  `json:"problems"`
}

type SkillResult struct {
	Roots []SkillRoot `json:"roots"`
}

type ExecutionTermination struct {
	Kind   string `json:"kind"`
	Code   *int   `json:"code"`
	Signal *int   `json:"signal"`
	Reason string `json:"reason,omitempty"`
}
type ExecutionResult struct {
	Termination ExecutionTermination `json:"termination"`
	EndedAt     *time.Time           `json:"endedAt"`
	Capture     string               `json:"capture"`
}
type ExecutionReceipt struct {
	HostID             string           `json:"hostId"`
	SessionID          string           `json:"sessionId"`
	Instance           string           `json:"instance"`
	RequestKey         string           `json:"requestKey"`
	KeyScope           string           `json:"keyScope"`
	Kind               string           `json:"kind"`
	Phase              string           `json:"phase"`
	Acceptance         string           `json:"acceptance"`
	LaunchOutcome      string           `json:"launchOutcome"`
	Publication        string           `json:"publication"`
	Removal            string           `json:"removal"`
	ReceiptRetainUntil *time.Time       `json:"receiptRetainUntil"`
	RetentionReason    string           `json:"retentionReason,omitempty"`
	Result             *ExecutionResult `json:"result"`
}

// ScopeSubject identifies the authenticated key without legacy output-bearing records.
type ScopeSubject struct {
	TargetID    string    `json:"targetId"`
	Principal   string    `json:"principal"`
	Class       string    `json:"class"`
	Permissions []string  `json:"permissions"`
	ExpiresAt   time.Time `json:"expiresAt"`
}
type ScopeHost struct {
	ID          string `json:"id"`
	MachineName string `json:"machineName"`
	Version     string `json:"version"`
}
type ScopeSession struct {
	ID             string     `json:"id"`
	HostID         string     `json:"hostId"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastAttachedAt *time.Time `json:"lastAttachedAt"`
	DetachedAt     *time.Time `json:"detachedAt"`
	ExitCode       *int       `json:"exitCode"`
}
type ScopeService struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Scope      string `json:"scope"`
	Configured bool   `json:"configured"`
}
type ScopeMetadata struct {
	Subject    ScopeSubject   `json:"subject"`
	Section    string         `json:"section"`
	ObservedAt time.Time      `json:"observedAt"`
	Freshness  string         `json:"freshness"`
	Snapshot   string         `json:"snapshot"`
	Host       *ScopeHost     `json:"host"`
	Sessions   []ScopeSession `json:"sessions"`
	Services   []ScopeService `json:"services"`
	NextPage   *string        `json:"nextPage"`
}
type ScopeEntry struct {
	Subject     string    `json:"subject"`
	Incarnation string    `json:"incarnation"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}
type ScopePolicy struct {
	Version  int          `json:"version"`
	Target   string       `json:"target"`
	Revision uint64       `json:"revision"`
	Entries  []ScopeEntry `json:"entries"`
}
type ScopeChange struct {
	Revision uint64      `json:"revision"`
	Changed  bool        `json:"changed"`
	Entry    *ScopeEntry `json:"entry"`
}
type ScopeCredential struct {
	Identity     string `json:"identity"`
	KeyDirectory string `json:"keyDirectory"`
}

type ScopeOutput struct {
	Subject       ScopeSubject       `json:"subject"`
	SessionID     string             `json:"sessionId"`
	Format        string             `json:"format"`
	Source        string             `json:"source"`
	ObservedAt    *time.Time         `json:"observedAt"`
	CheckpointAt  *time.Time         `json:"checkpointAt"`
	BytesBase64   *string            `json:"bytesBase64"`
	Preview       []string           `json:"preview"`
	StyledPreview []ScopePreviewLine `json:"styledPreview"`
}
type ScopePreviewLine struct {
	Runs []ScopePreviewRun `json:"runs,omitempty"`
}
type ScopePreviewRun struct {
	Text  string            `json:"text"`
	Style ScopePreviewStyle `json:"style,omitempty"`
}
type ScopePreviewColor struct {
	Kind  uint8  `json:"kind,omitempty"`
	Value uint32 `json:"value,omitempty"`
}
type ScopePreviewStyle struct {
	Foreground     ScopePreviewColor `json:"foreground,omitempty"`
	Background     ScopePreviewColor `json:"background,omitempty"`
	UnderlineColor ScopePreviewColor `json:"underlineColor,omitempty"`
	Bold           bool              `json:"bold,omitempty"`
	Faint          bool              `json:"faint,omitempty"`
	Italic         bool              `json:"italic,omitempty"`
	Reverse        bool              `json:"reverse,omitempty"`
	Strikethrough  bool              `json:"strikethrough,omitempty"`
	Underline      uint8             `json:"underline,omitempty"`
}
