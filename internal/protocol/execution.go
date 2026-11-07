package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TypeExecutionCreate  = "execution.create"
	TypeExecutionRequest = "execution.request"
	TypeExecutionReceipt = "execution.receipt"
	TypeExecutionLogs    = "execution.logs"
	TypeExecutionWait    = "execution.wait"
	TypeExecutionSignal  = "execution.signal"
	TypeExecutionKill    = "execution.kill"
	TypeExecutionRemove  = "execution.remove"
)

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
	Streams     []ExecutionStream    `json:"streams,omitempty"`
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
	OutputRetainUntil  *time.Time       `json:"outputRetainUntil"`
	OutputExpired      bool             `json:"outputExpired"`
	KillRequested      bool             `json:"killRequested"`
	Result             *ExecutionResult `json:"result"`
}

// validateExecutionRaw runs before Control decoding can discard request evidence.
func validateExecutionRaw(payload []byte) error {
	if !executionCandidate(payload) {
		return nil
	}
	if len(payload) > 40<<10 || !utf8.Valid(payload) {
		return fmt.Errorf("execution: invalid raw size or UTF-8")
	}
	scan := scanExecutionFields(payload)
	if !scan.execution {
		return nil
	}
	if scan.err != nil {
		return fmt.Errorf("execution: raw envelope: %w", scan.err)
	}
	if len(payload) > 40<<10 || !utf8.Valid(payload) || scan.duplicate {
		return fmt.Errorf("execution: invalid raw size, UTF-8 or duplicate field")
	}
	kind := scan.kind
	for name, raw := range scan.fields {
		if err := validateExecutionField(kind, name, raw); err != nil {
			return err
		}
	}
	return validateExecutionRequired(kind, scan.fields)
}

const executionTypeField = "type"

type executionFields struct {
	fields               map[string]json.RawMessage
	kind                 string
	execution, duplicate bool
	err                  error
}

func scanExecutionFields(payload []byte) executionFields {
	scan := executionFields{fields: make(map[string]json.RawMessage)}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	first, err := decoder.Token()
	if err != nil {
		scan.err = err
		return scan
	}
	if first != json.Delim('{') {
		return scan
	}
	for decoder.More() {
		if err := scan.takeField(decoder); err != nil {
			scan.err = err
			return scan
		}
	}
	_, scan.err = decoder.Token()
	if scan.err == nil && decoder.Decode(new(any)) != io.EOF {
		scan.err = fmt.Errorf("trailing JSON")
	}
	return scan
}
func (s *executionFields) takeField(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read field name: %w", err)
	}
	name, ok := token.(string)
	if !ok {
		return fmt.Errorf("invalid field name")
	}
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("read field value: %w", err)
	}
	if _, ok := s.fields[name]; ok {
		s.duplicate = true
	}
	s.fields[name] = raw
	if !strings.EqualFold(name, executionTypeField) {
		return nil
	}
	_ = json.Unmarshal(raw, &s.kind)
	s.execution = s.execution || isExecutionRequest(s.kind)
	return nil
}
func validateExecutionField(kind, name string, raw json.RawMessage) error {
	switch name {
	case executionTypeField, "requestId":
		return executionString(raw)
	case "requestKey":
		if kind != TypeExecutionCreate && kind != TypeExecutionRequest {
			return fmt.Errorf("execution: unexpected request key")
		}
		return executionString(raw)
	}
	if kind != TypeExecutionCreate {
		return validateExecutionBoundField(kind, name, raw)
	}
	switch name {
	case "kind", "cwd", "term":
		return executionString(raw)
	case "command":
		return executionArguments(raw)
	case "cols", "rows", "depth":
		return executionInteger(name, raw)
	default:
		return fmt.Errorf("execution: unexpected field %s", name)
	}
}
func executionArguments(raw []byte) error {
	var args []json.RawMessage
	if json.Unmarshal(raw, &args) != nil || args == nil || len(args) > 1024 {
		return fmt.Errorf("execution: invalid command array")
	}
	for _, arg := range args {
		if err := executionString(arg); err != nil {
			return err
		}
	}
	return nil
}
func executionInteger(name string, raw []byte) error {
	var n int
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &n) != nil || (name == "depth" && n != 0) {
		return fmt.Errorf("execution: invalid %s", name)
	}
	return nil
}
func executionString(raw []byte) error {
	var value string
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil || strings.ContainsRune(value, 0) {
		return fmt.Errorf("execution: invalid string")
	}
	return executionEscapes(raw)
}
func executionEscapes(raw []byte) error {
	for i := 0; i+5 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return fmt.Errorf("execution: Unicode escape: %w", err)
		}
		if code >= 0xdc00 && code <= 0xdfff {
			return fmt.Errorf("execution: unpaired surrogate")
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if err := executionLowSurrogate(raw, i); err != nil {
			return err
		}
		i += 10
	}
	return nil
}
func executionLowSurrogate(raw []byte, i int) error {
	if i+10 >= len(raw) || raw[i+5] != '\\' || raw[i+6] != 'u' {
		return fmt.Errorf("execution: unpaired surrogate")
	}
	low, err := strconv.ParseUint(string(raw[i+7:i+11]), 16, 16)
	if err != nil || low < 0xdc00 || low > 0xdfff {
		return fmt.Errorf("execution: unpaired surrogate")
	}
	return nil
}

type ExecutionValidationError struct{ Err error }

func (e *ExecutionValidationError) Error() string { return e.Err.Error() }
func (e *ExecutionValidationError) Unwrap() error { return e.Err }

// The discriminator keeps every type occurrence without copying unrelated catalog payloads.
// Full validation still runs against the original bytes before Control decoding.
type executionDiscriminator struct{ execution bool }

func (d *executionDiscriminator) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("execution discriminator: %w", err)
	}
	d.execution = d.execution || isExecutionRequest(value)
	return nil
}
func executionCandidate(payload []byte) bool {
	var probe struct {
		Type executionDiscriminator `json:"type"`
	}
	_ = json.Unmarshal(payload, &probe)
	return probe.Type.execution
}

type ExecutionStream struct {
	Stream      string `json:"stream"`
	Generation  string `json:"generation"`
	Earliest    string `json:"earliest"`
	End         string `json:"end"`
	ObservedEnd string `json:"observedEnd"`
	EOF         bool   `json:"eof"`
	CloseReason string `json:"closeReason"`
	Loss        string `json:"loss"`
}

type ExecutionPage struct {
	HostID         string `json:"hostId"`
	SessionID      string `json:"sessionId"`
	Instance       string `json:"instance"`
	Stream         string `json:"stream"`
	Generation     string `json:"generation"`
	Bytes          []byte `json:"bytesBase64"`
	StartCursor    string `json:"startCursor"`
	NextCursor     string `json:"nextCursor"`
	EarliestCursor string `json:"earliestCursor"`
	EndCursor      string `json:"endCursor"`
	ObservedEnd    string `json:"observedEnd"`
	StreamClosed   bool   `json:"streamClosed"`
	EOF            bool   `json:"eof"`
	CloseReason    string `json:"closeReason"`
	Loss           string `json:"loss"`
	Available      bool   `json:"available"`
	Gap            bool   `json:"gap"`
	Reason         string `json:"reason,omitempty"`
}

func isExecutionRequest(kind string) bool {
	switch kind {
	case TypeExecutionCreate, TypeExecutionRequest, TypeExecutionWait, TypeExecutionLogs, TypeExecutionSignal, TypeExecutionKill, TypeExecutionRemove:
		return true
	default:
		return false
	}
}
func validateExecutionBoundField(kind, name string, raw json.RawMessage) error {
	if kind == TypeExecutionRequest {
		return fmt.Errorf("execution: unexpected field %s", name)
	}
	switch name {
	case "sessionId", "instance":
		return executionString(raw)
	case "stream", "cursor":
		if kind == TypeExecutionLogs {
			return executionString(raw)
		}
	case "maxBytes":
		if kind == TypeExecutionLogs {
			return executionInteger(name, raw)
		}
	case "timeoutMillis":
		if kind == TypeExecutionWait {
			return executionInteger(name, raw)
		}
	case "signal":
		if kind == TypeExecutionSignal {
			return executionString(raw)
		}
	}
	return fmt.Errorf("execution: unexpected field %s", name)
}

func validateExecutionRequired(kind string, fields map[string]json.RawMessage) error {
	required := []string{executionTypeField, "requestId"}
	if kind == TypeExecutionCreate || kind == TypeExecutionRequest {
		required = append(required, "requestKey")
	} else {
		required = append(required, "sessionId", "instance")
	}
	if kind == TypeExecutionCreate {
		required = append(required, "kind", "command", "cwd")
	}
	for _, name := range required {
		if fields[name] == nil {
			return fmt.Errorf("execution: missing %s", name)
		}
	}
	return nil
}
