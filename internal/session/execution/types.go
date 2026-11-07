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
}

func (k Key) Validate() error {
	if k.Host == "" || k.Principal == "" || (k.Class != "device" && k.Class != "local-account") {
		return fmt.Errorf("execution: missing trusted namespace")
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
func (k Key) Hash() string { return digest([]string{k.Host, k.Class, k.Principal, k.Opaque}) }

type Intent struct {
	Kind    string   `json:"kind"`
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	Cols    int      `json:"cols"`
	Rows    int      `json:"rows"`
	Term    string   `json:"term"`
}

func Normalize(i Intent) (Intent, error) {
	if i.Kind != "pty" {
		return Intent{}, fmt.Errorf("execution: only explicit kind pty is supported")
	}
	if len(i.Command) == 0 || len(i.Command) > 1024 || i.Command[0] == "" {
		return Intent{}, fmt.Errorf("execution: require 1 to 1024 command arguments")
	}
	if len(i.Cwd) > 4096 || !filepath.IsAbs(i.Cwd) || filepath.Clean(i.Cwd) != i.Cwd {
		return Intent{}, fmt.Errorf("execution: cwd must be an absolute clean target path")
	}
	if i.Cols == 0 {
		i.Cols = 80
	}
	if i.Rows == 0 {
		i.Rows = 24
	}
	if i.Term == "" {
		i.Term = "dumb"
	}
	if i.Cols < 1 || i.Rows < 1 || i.Cols > 2048 || i.Rows > 2048 || i.Cols > (256<<10)/i.Rows {
		return Intent{}, fmt.Errorf("execution: invalid terminal dimensions")
	}
	for _, value := range append(append([]string(nil), i.Command...), i.Cwd, i.Term) {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return Intent{}, fmt.Errorf("execution: invalid command text")
		}
	}
	data, _ := json.Marshal(i)
	if len(data) > 32<<10 {
		return Intent{}, fmt.Errorf("execution: intent exceeds 32 KiB")
	}
	i.Command = append([]string(nil), i.Command...)
	return i, nil
}

type Binding struct {
	Host         string              `json:"host"`
	Reservation  session.Reservation `json:"reservation"`
	RequestHash  string              `json:"requestHash"`
	LaunchDigest string              `json:"launchDigest"`
}

func NewBinding(key Key, intent Intent, r session.Reservation, env []string) Binding {
	b := Binding{Host: key.Host, Reservation: r, RequestHash: key.Hash()}
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
	if b.Host == "" || b.Reservation.Purpose != session.Execution || !validDigest(b.RequestHash) || !validDigest(b.LaunchDigest) {
		return fmt.Errorf("execution: invalid binding")
	}
	return nil
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
	Binding     Binding     `json:"binding"`
	Termination Termination `json:"termination"`
	EndedAt     *time.Time  `json:"endedAt"`
	Capture     string      `json:"capture"`
}

func (r Result) validate(b Binding) error {
	if r.Binding != b || r.Capture != captureNotRecorded {
		return fmt.Errorf("execution: mismatched result")
	}
	t := r.Termination
	switch t.Kind {
	case "exit":
		if t.Code == nil || t.Signal != nil || *t.Code < 0 {
			return fmt.Errorf("execution: invalid exit")
		}
	case "signal":
		if t.Signal == nil || *t.Signal <= 0 || t.Code != nil {
			return fmt.Errorf("execution: invalid signal")
		}
	case notStarted, "interrupted":
		if t.Code != nil || t.Signal != nil || t.Reason == "" {
			return fmt.Errorf("execution: invalid non-exit")
		}
	default:
		return fmt.Errorf("execution: invalid termination")
	}
	return nil
}

type Receipt struct {
	RetentionReason string       `json:"-"`
	Binding         Binding      `json:"binding"`
	Key             Key          `json:"key"`
	IntentDigest    string       `json:"intentDigest"`
	Owner           string       `json:"owner"`
	Phase           string       `json:"phase"`
	Acceptance      string       `json:"acceptance"`
	Publication     string       `json:"publication"`
	LaunchOutcome   string       `json:"launchOutcome"`
	AcceptedAt      time.Time    `json:"acceptedAt"`
	RetainUntil     *time.Time   `json:"retainUntil"`
	TerminalAt      *time.Time   `json:"terminalAt"`
	Anchor          *ClockAnchor `json:"anchor"`
	Result          *Result      `json:"result"`
	Removal         string       `json:"removal"`
}

func (r Receipt) validate() error {
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	if err := r.Key.Validate(); err != nil {
		return err
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
	captureNotRecorded   = "not_recorded"
	notStarted           = "not_started"
	outcomeUnknown       = "unknown"
	outcomeStarted       = "started"
	removalPresent       = "present"
	removalRemoved       = "removed"
	publicationPublished = "published"
	pending              = "pending"
)
