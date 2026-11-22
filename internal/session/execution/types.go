package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/session"
)

var (
	ErrProgress  = fmt.Errorf("request launch publication is in progress")
	ErrConflict  = fmt.Errorf("request key already names a different command")
	ErrNotFound  = fmt.Errorf("no retained request evidence")
	ErrLimit     = fmt.Errorf("execution journal quota reached")
	ErrUnknown   = fmt.Errorf("request launch outcome is unknown")
	ErrProtected = fmt.Errorf("execution receipt protects this session")
)

type Key struct {
	Host      string `json:"host"`
	Class     string `json:"class"`
	Principal string `json:"principal"`
	Opaque    string `json:"opaque"`
	// Context and Operation scope an application caller's key: the same
	// caller key in two contexts, or for two operations, names two receipts.
	Context   string `json:"context,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// ApplicationClass keys belong to one verified application caller. Their
// Principal is the caller's application ID.
const ApplicationClass = "application"

// Operation scopes of application keys: a job created in a context, and a
// context opened on the host.
const (
	OperationExec        = "execution.create"
	OperationContextOpen = "context.open"
)

func (k Key) Validate() error {
	if k.Host == "" || k.Principal == "" || (k.Class != "device" && k.Class != "local-account" && k.Class != InternalServiceClass && k.Class != ApplicationClass) {
		return fmt.Errorf("execution: missing trusted namespace")
	}
	if (k.Class == ApplicationClass) != (k.Operation != "") || (k.Operation == OperationExec) != (k.Context != "") {
		return fmt.Errorf("execution: application keys need an operation, and jobs a context")
	}
	if len(k.Opaque) < 1 || len(k.Opaque) > 128 {
		return fmt.Errorf("execution: request key must contain 1 to 128 ASCII bytes")
	}
	for _, ch := range []byte(k.Opaque) {
		if ch < 0x21 || ch > 0x7e {
			return fmt.Errorf("execution: invalid request key")
		}
	}
	return nil
}
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Hash keeps the four-field form for existing namespaces, so receipts stored
// before contexts existed stay addressable.
func (k Key) Hash() string {
	if k.Context == "" && k.Operation == "" {
		return digest([]string{k.Host, k.Class, k.Principal, k.Opaque})
	}
	return digest([]string{k.Host, k.Class, k.Principal, k.Opaque, k.Context, k.Operation})
}

// Digest is the canonical SHA-256 of a JSON value, used for intents.
func Digest(value any) string { return digest(value) }

type Intent struct {
	Kind    string   `json:"kind"`
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Term    string   `json:"term"`
	Stdin   string   `json:"stdin,omitempty"`
}

const InternalServiceClass = "internal-service"

const (
	KindPTY      = "pty"
	KindPipe     = "pipe"
	KindLine     = "line"
	StdinOpen    = "open"
	StdinClosed  = "closed"
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

func Normalize(i Intent) (Intent, error) {
	if err := validateIntentInput(i); err != nil {
		return Intent{}, err
	}
	if i.Kind == KindPTY {
		i = terminalDefaults(i)
	} else if i.Stdin == "" {
		i.Stdin = StdinClosed
	}
	data, _ := json.Marshal(i)
	if len(data) > 32<<10 {
		return Intent{}, fmt.Errorf("execution: intent exceeds 32 KiB")
	}
	i.Command = append([]string(nil), i.Command...)
	return i, nil
}

func terminalDefaults(i Intent) Intent {
	if i.Cols == 0 {
		i.Cols = 80
	}
	if i.Rows == 0 {
		i.Rows = 24
	}
	if i.Term == "" {
		i.Term = "dumb"
	}
	return i
}

type Binding struct {
	Host         string              `json:"host"`
	Reservation  session.Reservation `json:"reservation"`
	RequestHash  string              `json:"requestHash"`
	LaunchDigest string              `json:"launchDigest"`
	Kind         string              `json:"kind,omitempty"`
	Stdin        string              `json:"stdin,omitempty"`
}

func NewBinding(key Key, intent Intent, r session.Reservation, env []string) Binding {
	b := Binding{Host: key.Host, Reservation: r, RequestHash: key.Hash()}
	if intent.Kind != KindPTY {
		b.Kind, b.Stdin = intent.Kind, intent.Stdin
	}
	b.LaunchDigest = digest(struct {
		Key    Key
		Intent Intent
		Ref    session.Reservation
		Env    string
	}{key, intent, r, digest(env)})
	return b
}
func (b Binding) Validate() error {
	if err := b.Reservation.Validate(); err != nil {
		return executionBoundaryError("types.go Validate", err)
	}
	if b.Host == "" || b.Reservation.Purpose != PurposeForKind(b.Mode()) || !validDigest(b.RequestHash) || !validDigest(b.LaunchDigest) {
		return fmt.Errorf("execution: invalid binding")
	}
	return validateStdin(b.Mode(), b.Stdin)
}

func PurposeForKind(kind string) session.Purpose {
	switch kind {
	case KindPipe:
		return session.PipeExecution
	case KindLine:
		return session.LineExecution
	default:
		return session.Execution
	}
}

func (b Binding) Mode() string {
	if b.Kind == "" {
		return KindPTY
	}
	return b.Kind
}

func (b Binding) Streams() []string {
	switch b.Mode() {
	case KindPipe:
		return []string{StreamStdout, StreamStderr}
	case KindLine:
		return []string{StreamStderr}
	default:
		return []string{KindPTY}
	}
}
func validDigest(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == s
}

type Termination struct {
	Kind   string `json:"kind"`
	Code   *int   `json:"code"`
	Signal *int   `json:"signal"`
	Reason string `json:"reason,omitempty"`
}
type Result struct {
	Binding     Binding        `json:"binding"`
	Termination Termination    `json:"termination"`
	EndedAt     *time.Time     `json:"endedAt"`
	Capture     string         `json:"capture"`
	Streams     []StreamResult `json:"streams,omitempty"`
}

func (r Result) validate(b Binding) error {
	if r.Binding != b {
		return fmt.Errorf("execution: mismatched result")
	}
	if err := r.validateCapture(); err != nil {
		return err
	}
	return validateTermination(r.Termination)
}

func validateTermination(t Termination) error {
	switch t.Kind {
	case "exit":
		if t.Code == nil || t.Signal != nil || *t.Code < 0 {
			return fmt.Errorf("execution: invalid exit")
		}
	case "killed":
		return validateKilledTermination(t)
	case "signal":
		if t.Signal == nil || *t.Signal <= 0 || t.Code != nil {
			return fmt.Errorf("execution: invalid signal")
		}
	case notStarted, interrupted:
		if t.Code != nil || t.Signal != nil || t.Reason == "" {
			return fmt.Errorf("execution: invalid non-exit")
		}
	default:
		return fmt.Errorf("execution: invalid termination")
	}
	return nil
}

type Receipt struct {
	RetentionReason   string       `json:"-"`
	OutputRetainUntil *time.Time   `json:"outputRetainUntil"`
	OutputExpired     bool         `json:"outputExpired"`
	KillRequested     bool         `json:"killRequested"`
	Binding           Binding      `json:"binding"`
	Key               Key          `json:"key"`
	IntentDigest      string       `json:"intentDigest"`
	Owner             string       `json:"owner"`
	Phase             string       `json:"phase"`
	Acceptance        string       `json:"acceptance"`
	Publication       string       `json:"publication"`
	LaunchOutcome     string       `json:"launchOutcome"`
	AcceptedAt        time.Time    `json:"acceptedAt"`
	RetainUntil       *time.Time   `json:"retainUntil"`
	TerminalAt        *time.Time   `json:"terminalAt"`
	Anchor            *ClockAnchor `json:"anchor"`
	Result            *Result      `json:"result"`
	Removal           string       `json:"removal"`
	// Context and LaunchIntentDigest are set for context jobs. IntentDigest
	// then covers the caller-supplied request, which decides replays, and
	// LaunchIntentDigest the intent resolved against the reserved revision.
	Context            *ContextUse `json:"context,omitempty"`
	LaunchIntentDigest string      `json:"launchIntentDigest,omitempty"`
	Cwd                string      `json:"cwd,omitempty"`
}

// ContextUse names the context revision a job was reserved against.
type ContextUse struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

func (r Receipt) launchIntentDigest() string {
	if r.LaunchIntentDigest != "" {
		return r.LaunchIntentDigest
	}
	return r.IntentDigest
}

func (r Receipt) validateContext() error {
	if r.Key.Operation != OperationExec {
		if r.Context != nil || r.LaunchIntentDigest != "" {
			return fmt.Errorf("execution: context evidence outside a context job")
		}
		return nil
	}
	if r.Context == nil || r.Context.ID != r.Key.Context || r.Context.Revision == 0 || !validDigest(r.LaunchIntentDigest) {
		return fmt.Errorf("execution: damaged context job receipt")
	}
	return nil
}

func (r Receipt) validate() error {
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	if err := r.Key.Validate(); err != nil {
		return err
	}
	if err := r.validateContext(); err != nil {
		return err
	}
	if (r.Binding.Mode() == KindLine) != (r.Key.Class == InternalServiceClass) {
		return fmt.Errorf("execution: invalid internal request namespace")
	}
	if r.Binding.RequestHash != r.Key.Hash() || r.Binding.Host != r.Key.Host || !validDigest(r.IntentDigest) || r.Owner == "" || r.AcceptedAt.IsZero() {
		return fmt.Errorf("execution: damaged receipt")
	}
	if r.Acceptance != "durable" || (r.Publication != pending && r.Publication != publicationPublished) {
		return fmt.Errorf("execution: invalid receipt observation")
	}
	if err := r.validatePhase(); err != nil {
		return err
	}
	if r.Removal != removalPresent && r.Removal != pending && r.Removal != removalRemoved {
		return fmt.Errorf("execution: invalid removal state")
	}
	return nil
}
func (r Receipt) validatePhase() error {
	switch r.Phase {
	case phaseReserved, phaseDispatched, phaseLive, phaseUncertain:
		if r.Result != nil || r.RetainUntil != nil || r.TerminalAt != nil || r.Anchor != nil {
			return fmt.Errorf("execution: contradictory active receipt")
		}
	case phaseTerminal:
		if r.Result == nil || r.TerminalAt == nil || r.Anchor == nil {
			return fmt.Errorf("execution: missing terminal result")
		}
		if err := r.Result.validate(r.Binding); err != nil {
			return err
		}
	default:
		return fmt.Errorf("execution: invalid receipt phase")
	}
	return nil
}
func attempt(b Binding, name string) string {
	return filepath.Join("execution", "attempts", b.Reservation.Instance, name)
}
func requestName(hash string) string { return filepath.Join("execution", "requests", hash+".json") }

const (
	phaseReserved        = "reserved"
	phaseDispatched      = "dispatched"
	phaseLive            = "live"
	phaseUncertain       = "uncertain"
	phaseTerminal        = "terminal"
	CaptureNotRecorded   = "not_recorded"
	notStarted           = "not_started"
	interrupted          = "interrupted"
	outcomeUnknown       = "unknown"
	outcomeStarted       = "started"
	removalPresent       = "present"
	removalRemoved       = "removed"
	publicationPublished = "published"
	pending              = "pending"
)

func validIntentText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validateIntentInput(i Intent) error {
	if err := validateStdin(i.Kind, i.Stdin); err != nil {
		return err
	}
	if err := validateIntentTerminal(i); err != nil {
		return err
	}
	if len(i.Command) == 0 || len(i.Command) > 1024 || i.Command[0] == "" {
		return fmt.Errorf("execution: require 1 to 1024 command arguments")
	}
	if len(i.Cwd) > 4096 || !filepath.IsAbs(i.Cwd) || filepath.Clean(i.Cwd) != i.Cwd {
		return fmt.Errorf("execution: cwd must be an absolute clean target path")
	}
	if !validIntentText(i.Cwd) || !validIntentText(i.Term) {
		return fmt.Errorf("execution: invalid command text")
	}
	for _, value := range i.Command {
		if !validIntentText(value) {
			return fmt.Errorf("execution: invalid command text")
		}
	}
	return nil
}

func validateIntentTerminal(i Intent) error {
	if i.Kind != KindPTY {
		if i.Cols != 0 || i.Rows != 0 || i.Term != "" {
			return fmt.Errorf("execution: terminal settings require kind pty")
		}
		return nil
	}
	i = terminalDefaults(i)
	if i.Cols < 1 || i.Rows < 1 || i.Cols > 2048 || i.Rows > 2048 || i.Cols > (256<<10)/i.Rows {
		return fmt.Errorf("execution: invalid terminal dimensions")
	}
	return nil
}

func validateStdin(kind, mode string) error {
	switch kind {
	case KindPTY:
		if mode == "" {
			return nil
		}
	case KindPipe:
		if mode == "" || mode == StdinOpen || mode == StdinClosed {
			return nil
		}
	case KindLine:
		if mode == StdinOpen {
			return nil
		}
	default:
		return fmt.Errorf("execution: invalid kind")
	}
	return fmt.Errorf("execution: invalid stdin mode")
}

func (r Result) validateCapture() error {
	if r.Capture == CaptureNotRecorded && len(r.Streams) == 0 {
		return nil
	}
	if r.Capture != "complete" && r.Capture != "incomplete" {
		return fmt.Errorf("execution: invalid capture result")
	}
	streams := r.Binding.Streams()
	if len(r.Streams) != len(streams) {
		return fmt.Errorf("execution: missing stream result")
	}
	for index, stream := range streams {
		if err := validateFinalStream(r.Streams[index], stream, r.Capture == "complete"); err != nil {
			return err
		}
	}
	return nil
}

func validateFinalStream(s StreamResult, stream string, complete bool) error {
	earliest, e1 := parseOffset(s.Earliest)
	end, e2 := parseOffset(s.End)
	observed, e3 := parseOffset(s.ObservedEnd)
	if e1 != nil || e2 != nil || e3 != nil || earliest > end || end > observed || !validToken(s.Generation) || s.Stream != stream {
		return fmt.Errorf("execution: invalid final capture range")
	}
	if err := validStreamClosure(true, s.EOF, s.CloseReason, s.Loss); err != nil {
		return err
	}
	if complete && (!s.EOF || (s.Loss != "" && s.Loss != captureRetention)) {
		return fmt.Errorf("execution: contradictory complete capture")
	}
	return nil
}

func validateKilledTermination(t Termination) error {
	if (t.Signal == nil) == (t.Code == nil) {
		return fmt.Errorf("execution: invalid killed result")
	}
	if t.Signal != nil && *t.Signal <= 0 {
		return fmt.Errorf("execution: invalid kill signal")
	}
	if t.Code != nil && *t.Code < 0 {
		return fmt.Errorf("execution: invalid kill exit")
	}
	return nil
}
