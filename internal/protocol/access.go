package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/session"
)

const (
	TypeAccessRead         = "access.read"
	TypeAccessResult       = "access.read.result"
	TypeAccessDenied       = "access.denied"
	MaximumAccessRequest   = 16 << 10
	MaximumAccessReply     = 256 << 10
	MaximumAccessCursor    = 2048
	MaximumAccessRequestID = 128
)

type AccessRead struct {
	Section   string `json:"section"`
	SessionID string `json:"sessionId,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
type AccessRequest struct {
	Type      string     `json:"type"`
	RequestID string     `json:"requestId"`
	Read      AccessRead `json:"accessRead"`
}
type AccessSubject struct {
	TargetID    string    `json:"targetId"`
	Principal   string    `json:"principal"`
	Class       string    `json:"class"`
	Permissions []string  `json:"permissions"`
	ExpiresAt   time.Time `json:"expiresAt"`
}
type AccessHost struct {
	ID          string `json:"id"`
	MachineName string `json:"machineName"`
	Version     string `json:"version"`
}
type AccessSession struct {
	ID             string     `json:"id"`
	HostID         string     `json:"hostId"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastAttachedAt *time.Time `json:"lastAttachedAt"`
	DetachedAt     *time.Time `json:"detachedAt"`
	ExitCode       *int       `json:"exitCode"`
}
type AccessService struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Scope      string `json:"scope"`
	Configured bool   `json:"configured"`
}
type AccessReadResult struct {
	Subject    AccessSubject   `json:"subject"`
	Section    string          `json:"section"`
	ObservedAt time.Time       `json:"observedAt"`
	Freshness  string          `json:"freshness"`
	Snapshot   string          `json:"snapshot"`
	Host       *AccessHost     `json:"host"`
	Sessions   []AccessSession `json:"sessions"`
	Services   []AccessService `json:"services"`
	NextPage   *string         `json:"nextPage"`
}
type AccessReply struct {
	Type      string            `json:"type"`
	RequestID string            `json:"requestId"`
	Result    *AccessReadResult `json:"accessResult,omitempty"`
	ErrorCode string            `json:"errorCode,omitempty"`
	Message   string            `json:"message,omitempty"`
}

// rawAccessObject keeps presence and duplicate-key evidence before typed decoding.
func rawAccessObject(data []byte, maximum int, allowed ...string) (map[string]json.RawMessage, error) {
	if len(data) == 0 || len(data) > maximum || !utf8.Valid(data) {
		return nil, errors.New("protocol: invalid access JSON size or encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("protocol: access object required")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		if err := readAccessField(decoder, fields, allowed); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("protocol: close access object: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("protocol: trailing access data")
	}
	return fields, nil
}
func readAccessField(decoder *json.Decoder, fields map[string]json.RawMessage, allowed []string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("protocol: access field: %w", err)
	}
	name, ok := token.(string)
	if !ok || fields[name] != nil {
		return errors.New("protocol: duplicate access field")
	}
	permitted := false
	for _, candidate := range allowed {
		if candidate == name {
			permitted = true
		}
	}
	if !permitted {
		return errors.New("protocol: unknown access field")
	}
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("protocol: access field value: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return errors.New("protocol: null access field")
	}
	fields[name] = value
	return nil
}
func DecodeAccessRead(data []byte) (AccessRequest, error) {
	fields, err := rawAccessObject(data, MaximumAccessRequest, "type", "requestId", "accessRead")
	if err != nil {
		return AccessRequest{}, err
	}
	var request AccessRequest
	if len(fields) != 3 || json.Unmarshal(fields["type"], &request.Type) != nil || request.Type != TypeAccessRead {
		return request, errors.New("protocol: access.read required")
	}
	if json.Unmarshal(fields["requestId"], &request.RequestID) != nil || !accessRequestID(request.RequestID) {
		return AccessRequest{}, errors.New("protocol: bounded access request ID required")
	}
	payload, err := rawAccessObject(fields["accessRead"], MaximumAccessRequest, "section", "sessionId", "limit", "cursor")
	if err != nil {
		return request, err
	}
	if err := json.Unmarshal(fields["accessRead"], &request.Read); err != nil {
		return request, fmt.Errorf("protocol: decode access read: %w", err)
	}
	if err := validateAccessRead(request.Read, payload); err != nil {
		return request, err
	}
	return request, nil
}
func accessRequestID(id string) bool {
	if len(id) < 1 || len(id) > MaximumAccessRequestID {
		return false
	}
	for _, value := range []byte(id) {
		if value < 33 || value > 126 {
			return false
		}
	}
	return true
}
func validateAccessRead(read AccessRead, fields map[string]json.RawMessage) error {
	if read.Section == TopicHost {
		if len(fields) != 1 {
			return errors.New("protocol: host read accepts no selectors")
		}
		return nil
	}
	if read.Section != TopicSessions && read.Section != TopicServices {
		return errors.New("protocol: unknown access section")
	}
	if _, present := fields["limit"]; present && (read.Limit < 1 || read.Limit > 500) {
		return errors.New("protocol: access limit must be 1 to 500")
	}
	if _, present := fields["cursor"]; present && (read.Cursor == "" || len(read.Cursor) > MaximumAccessCursor) {
		return errors.New("protocol: invalid access cursor size")
	}
	if _, present := fields["sessionId"]; !present {
		return nil
	}
	canonical, err := session.ParseID(read.SessionID)
	if read.Section != TopicSessions || err != nil || canonical != read.SessionID || read.Cursor != "" {
		return errors.New("protocol: invalid exact session selector")
	}
	return nil
}

type AccessCursor struct {
	Target      string `json:"target"`
	Subject     string `json:"subject"`
	Incarnation string `json:"incarnation"`
	Section     string `json:"section"`
	Last        string `json:"last"`
	SessionID   string `json:"sessionId"`
}

func (cursor AccessCursor) Encode() (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("protocol: encode access cursor: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	if len(token) > MaximumAccessCursor {
		return "", errors.New("protocol: access cursor exceeds size limit")
	}
	return token, nil
}
func DecodeAccessCursor(token string) (AccessCursor, error) {
	var cursor AccessCursor
	if len(token) > MaximumAccessCursor {
		return cursor, errors.New("protocol: access cursor exceeds size limit")
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(data) != token {
		return cursor, errors.New("protocol: invalid access cursor encoding")
	}
	fields, err := rawAccessObject(data, MaximumAccessCursor, "target", "subject", "incarnation", "section", "last", "sessionId")
	if err != nil || len(fields) != 6 {
		return cursor, errors.New("protocol: invalid access cursor fields")
	}
	if err := json.Unmarshal(data, &cursor); err != nil {
		return cursor, fmt.Errorf("protocol: decode access cursor: %w", err)
	}
	if cursor.Last == "" || len(cursor.Last) > 256 {
		return cursor, errors.New("protocol: invalid access cursor last key")
	}
	return cursor, nil
}

func DecodeAccessReply(data []byte) (AccessReply, error) {
	var reply AccessReply
	if len(data) > MaximumAccessReply || !utf8.Valid(data) {
		return reply, errors.New("protocol: invalid scoped reply size or encoding")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	if err := scanAccessReplyValue(scanner, 0); err != nil {
		return reply, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return reply, fmt.Errorf("protocol: decode scoped reply: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return reply, errors.New("protocol: trailing scoped reply")
	}
	if !accessRequestID(reply.RequestID) {
		return reply, errors.New("protocol: invalid scoped reply ID")
	}
	if reply.Type == TypeAccessDenied {
		if reply.Result != nil || reply.ErrorCode == "" || len(reply.Message) > 128 {
			return reply, errors.New("protocol: invalid scoped error")
		}
		return reply, nil
	}
	if reply.Type != TypeAccessResult || reply.Result == nil || reply.Message != "" || reply.ErrorCode != "" {
		return reply, errors.New("protocol: invalid scoped result")
	}
	if err := validateAccessResult(*reply.Result); err != nil {
		return reply, err
	}
	return reply, nil
}
func validateAccessResult(result AccessReadResult) error {
	if err := validateAccessObservation(result); err != nil {
		return err
	}
	if result.Section == TopicHost {
		if result.Host == nil || result.Host.ID != result.Subject.TargetID || len(result.Sessions) != 0 || len(result.Services) != 0 || result.NextPage != nil {
			return errors.New("protocol: invalid scoped host variant")
		}
		return nil
	}
	if result.Host != nil || len(result.Sessions) > 500 || len(result.Services) > 500 {
		return errors.New("protocol: invalid scoped row variant")
	}
	if result.Section == TopicSessions && len(result.Services) == 0 {
		return validateAccessSessions(result)
	}
	if result.Section != TopicServices || len(result.Sessions) != 0 {
		return errors.New("protocol: invalid scoped section")
	}
	return validateAccessServices(result.Services)
}
func validateAccessServices(rows []AccessService) error {
	for _, row := range rows {
		if row.Name == "" || (row.Kind != "static" && row.Kind != "files" && row.Kind != "proxy") || (row.Scope != "local" && row.Scope != "tailnet") || !row.Configured {
			return errors.New("protocol: invalid scoped service")
		}
	}
	return nil
}
func validateAccessSessions(result AccessReadResult) error {
	for _, row := range result.Sessions {
		id, err := session.ParseID(row.ID)
		if err != nil || id != row.ID || row.HostID != result.Subject.TargetID || row.CreatedAt.IsZero() {
			return errors.New("protocol: invalid scoped session")
		}
		switch row.State {
		case "running", "detached", "exited", "interrupted":
		default:
			return errors.New("protocol: invalid scoped session state")
		}
	}
	return nil
}
func scanAccessReplyValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("protocol: scoped reply JSON is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("protocol: scoped reply token: %w", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter == '{' {
		return scanAccessReplyObject(decoder, depth+1)
	}
	if delimiter != '[' {
		return errors.New("protocol: invalid scoped reply delimiter")
	}
	for decoder.More() {
		if err := scanAccessReplyValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	if err != nil {
		return fmt.Errorf("protocol: scoped reply array: %w", err)
	}
	return nil
}
func scanAccessReplyObject(decoder *json.Decoder, depth int) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("protocol: scoped reply key: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] || !accessReplyField(key) {
			return errors.New("protocol: duplicate scoped reply field")
		}
		seen[key] = true
		if err := scanAccessReplyValue(decoder, depth); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("protocol: scoped reply object: %w", err)
	}
	return nil
}

func accessReplyField(key string) bool {
	switch key {
	case "type", "requestId", "accessResult", "errorCode", "message", "subject", "targetId", "principal", "class", "permissions", "expiresAt", "section", "observedAt", "freshness", "snapshot", "host", "sessions", "services", "nextPage", "id", "machineName", "version", "hostId", "state", "createdAt", "lastAttachedAt", "detachedAt", "exitCode", "name", "kind", "scope", "configured", "accessOutputResult", "sessionId", "format", "source", "checkpointAt", "bytesBase64", "preview", "styledPreview", "runs", "text", "style", "foreground", "background", "underlineColor", "bold", "faint", "italic", "reverse", "strikethrough", "underline", "value":
		return true
	}
	return false
}

func validateAccessObservation(result AccessReadResult) error {
	if err := validateAccessSubject(result.Subject); err != nil {
		return err
	}
	if result.ObservedAt.IsZero() || result.Freshness != "live" || result.Snapshot != "non-atomic" || result.Sessions == nil || result.Services == nil {
		return errors.New("protocol: invalid scoped observation")
	}

	return nil
}

func (reply AccessReply) Encode() ([]byte, error) {
	if reply.Type == TypeAccessResult {
		if reply.Result == nil {
			return nil, errors.New("protocol: scoped result required")
		}
		if err := validateAccessResult(*reply.Result); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode scoped reply: %w", err)
	}
	if len(data) > MaximumAccessReply {
		return nil, errors.New("protocol: scoped reply exceeds size limit")
	}
	return data, nil
}

func accessIdentity(value string) bool {
	data, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(data) == 32 && base64.RawURLEncoding.EncodeToString(data) == value
}

const (
	AccessTail             = "tail"
	AccessPreview          = "preview"
	TypeAccessOutput       = "access.output"
	TypeAccessOutputResult = "access.output.result"
	// A maximal base64 tail plus the fixed subject and request envelope.
	MaximumAccessOutputReply = (MaxLogTail+2)/3*4 + 4096
)

// ErrAccessOutputLimit marks a valid output projection that exceeds its response budget.
var ErrAccessOutputLimit = errors.New("protocol: scoped output exceeds response limit")

type AccessOutput struct {
	SessionID string `json:"sessionId"`
	Format    string `json:"format"`
	MaxBytes  int    `json:"maxBytes,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}
type AccessOutputRequest struct {
	Type      string       `json:"type"`
	RequestID string       `json:"requestId"`
	Output    AccessOutput `json:"accessOutput"`
}
type AccessOutputResult struct {
	Subject       AccessSubject `json:"subject"`
	SessionID     string        `json:"sessionId"`
	Format        string        `json:"format"`
	Source        string        `json:"source"`
	ObservedAt    *time.Time    `json:"observedAt"`
	CheckpointAt  *time.Time    `json:"checkpointAt"`
	BytesBase64   *string       `json:"bytesBase64"`
	Preview       []string      `json:"preview"`
	StyledPreview []PreviewLine `json:"styledPreview"`
}
type AccessOutputReply struct {
	Type      string              `json:"type"`
	RequestID string              `json:"requestId"`
	Result    *AccessOutputResult `json:"accessOutputResult,omitempty"`
	ErrorCode string              `json:"errorCode,omitempty"`
	Message   string              `json:"message,omitempty"`
}

func AccessRequestType(data []byte) (string, error) {
	fields, err := rawAccessObject(data, MaximumAccessRequest, "type", "requestId", "accessRead", "accessOutput")
	if err != nil {
		return "", err
	}
	var name string
	if err := json.Unmarshal(fields["type"], &name); err != nil {
		return "", fmt.Errorf("protocol: access type: %w", err)
	}
	switch name {
	case TypeAccessRead:
		_, err = DecodeAccessRead(data)
	case TypeAccessOutput:
		_, err = DecodeAccessOutput(data)
	default:
		return "", errors.New("protocol: unsupported access request")
	}
	return name, err
}
func DecodeAccessOutput(data []byte) (AccessOutputRequest, error) {
	var request AccessOutputRequest
	fields, err := rawAccessObject(data, MaximumAccessRequest, "type", "requestId", "accessOutput")
	if err != nil {
		return request, err
	}
	if len(fields) != 3 || json.Unmarshal(fields["type"], &request.Type) != nil || request.Type != TypeAccessOutput {
		return request, errors.New("protocol: access.output required")
	}
	if json.Unmarshal(fields["requestId"], &request.RequestID) != nil || !accessRequestID(request.RequestID) {
		return request, errors.New("protocol: bounded access request ID required")
	}
	payload, err := rawAccessObject(fields["accessOutput"], MaximumAccessRequest, "sessionId", "format", "maxBytes", "cols", "rows")
	if err != nil {
		return request, err
	}
	if err := json.Unmarshal(fields["accessOutput"], &request.Output); err != nil {
		return request, fmt.Errorf("protocol: decode access output: %w", err)
	}
	if err := validateAccessOutput(request.Output, payload); err != nil {
		return request, err
	}
	if request.Output.Format == AccessTail && request.Output.MaxBytes == 0 {
		request.Output.MaxBytes = DefaultLogTail
	}
	if request.Output.Format == AccessPreview && request.Output.Cols == 0 {
		request.Output.Cols = 80
	}
	if request.Output.Format == AccessPreview && request.Output.Rows == 0 {
		request.Output.Rows = 24
	}
	return request, nil
}
func validateAccessOutput(output AccessOutput, fields map[string]json.RawMessage) error {
	id, err := session.ParseID(output.SessionID)
	if err != nil || id != output.SessionID {
		return errors.New("protocol: exact output session ID required")
	}
	switch output.Format {
	case AccessTail:
		return validateAccessTailRequest(output, fields)
	case AccessPreview:
		return validateAccessPreviewRequest(output, fields)
	default:
		return errors.New("protocol: unknown output format")
	}
}
func validateAccessTailRequest(output AccessOutput, fields map[string]json.RawMessage) error {
	if fields["cols"] != nil || fields["rows"] != nil {
		return errors.New("protocol: tail accepts no dimensions")
	}
	if fields["maxBytes"] != nil && (output.MaxBytes < 1 || output.MaxBytes > MaxLogTail) {
		return errors.New("protocol: invalid output tail size")
	}
	return nil
}
func validateAccessPreviewRequest(output AccessOutput, fields map[string]json.RawMessage) error {
	if fields["maxBytes"] != nil {
		return errors.New("protocol: preview accepts no tail size")
	}
	if fields["cols"] != nil && (output.Cols < 1 || output.Cols > MaxInspectionPreviewCols) {
		return errors.New("protocol: invalid output columns")
	}
	if fields["rows"] != nil && (output.Rows < 1 || output.Rows > MaxInspectionPreviewRows) {
		return errors.New("protocol: invalid output rows")
	}
	return nil
}

func validateAccessSubject(subject AccessSubject) error {
	if !accessIdentity(subject.TargetID) || !accessIdentity(subject.Principal) || subject.TargetID == subject.Principal || subject.Class != "scoped" || subject.ExpiresAt.IsZero() {
		return errors.New("protocol: invalid scoped subject")
	}
	if len(subject.Permissions) == 1 && subject.Permissions[0] == "metadata" {
		return nil
	}
	if len(subject.Permissions) == 2 && subject.Permissions[0] == "metadata" && subject.Permissions[1] == "session-output" {
		return nil
	}
	return errors.New("protocol: invalid scoped permissions")
}
func ValidateAccessOutputResult(result AccessOutputResult, request AccessOutput) error {
	if err := validateAccessSubject(result.Subject); err != nil {
		return err
	}
	if len(result.Subject.Permissions) != 2 || result.SessionID != request.SessionID || result.Format != request.Format || result.Preview == nil || result.StyledPreview == nil {
		return errors.New("protocol: invalid scoped output binding or arrays")
	}
	if result.Format == AccessTail {
		return validateAccessTail(result, request.MaxBytes)
	}
	if result.BytesBase64 != nil || len(result.Preview) > request.Rows {
		return errors.New("protocol: invalid scoped preview variant")
	}
	if result.Source == "live-preview" && result.ObservedAt != nil && !result.ObservedAt.IsZero() && result.CheckpointAt == nil {
		return validateAccessPreview(result, request)
	}
	if result.Source == "saved-preview" && result.ObservedAt == nil && result.CheckpointAt != nil && !result.CheckpointAt.IsZero() && len(result.StyledPreview) == 0 {
		return validateAccessPreview(result, request)
	}
	return errors.New("protocol: invalid preview source or time")
}
func validateAccessTail(result AccessOutputResult, maximum int) error {
	if result.Source != "live-tail" && result.Source != "saved-tail" && result.Source != "tail" {
		return errors.New("protocol: invalid tail source")
	}
	if result.BytesBase64 == nil || result.ObservedAt == nil || result.ObservedAt.IsZero() || result.CheckpointAt != nil || len(result.Preview) != 0 || len(result.StyledPreview) != 0 {
		return errors.New("protocol: invalid tail variant")
	}
	if len(*result.BytesBase64) > base64.StdEncoding.EncodedLen(maximum) {
		return errors.New("protocol: tail exceeds byte limit")
	}
	if strings.ContainsAny(*result.BytesBase64, "\r\n") {
		return errors.New("protocol: invalid tail encoding")
	}
	count, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(*result.BytesBase64)))
	if err != nil || count > int64(maximum) {
		return errors.New("protocol: invalid bounded tail encoding")
	}
	return nil
}
func validateAccessPreview(result AccessOutputResult, request AccessOutput) error {
	textBytes := 0
	for _, line := range result.Preview {
		if !utf8.ValidString(line) || ansi.StringWidth(line) > request.Cols {
			return errors.New("protocol: preview exceeds requested width or encoding")
		}
		textBytes += len(line)
	}
	for _, line := range result.StyledPreview {
		for _, run := range line.Runs {
			textBytes += len(run.Text)
		}
	}
	if textBytes > MaxInspectionTextBytes {
		return fmt.Errorf("%w: preview text", ErrAccessOutputLimit)
	}
	// The existing inspection validator owns control-free text and safe styles.
	return ValidateSessionInspection(SessionInspection{ObservedAt: time.Unix(1, 0).UTC(), Preview: result.Preview, StyledPreview: result.StyledPreview})
}
func (reply AccessOutputReply) Encode(request AccessOutput) ([]byte, error) {
	if reply.Type != TypeAccessOutputResult || reply.Result == nil || !accessRequestID(reply.RequestID) || reply.ErrorCode != "" || reply.Message != "" {
		return nil, errors.New("protocol: invalid output reply")
	}
	if err := ValidateAccessOutputResult(*reply.Result, request); err != nil {
		return nil, err
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode output reply: %w", err)
	}
	if len(data) > accessOutputReplyLimit(request.Format) {
		return nil, fmt.Errorf("%w: encoded reply", ErrAccessOutputLimit)
	}
	return data, nil
}
func accessOutputReplyLimit(format string) int {
	if format == AccessTail {
		return MaximumAccessOutputReply
	}
	return MaximumAccessReply
}
func DecodeAccessOutputReply(data []byte, request AccessOutput) (AccessOutputReply, error) {
	var reply AccessOutputReply
	if len(data) > accessOutputReplyLimit(request.Format) || !utf8.Valid(data) {
		return reply, errors.New("protocol: invalid output reply size or encoding")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	if err := scanAccessReplyValue(scanner, 0); err != nil {
		return reply, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return reply, fmt.Errorf("protocol: decode output reply: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return reply, errors.New("protocol: trailing output reply")
	}
	if reply.Type == TypeAccessDenied {
		_, err := DecodeAccessReply(data)
		return reply, err
	}
	if reply.Type != TypeAccessOutputResult || reply.Result == nil || !accessRequestID(reply.RequestID) || reply.ErrorCode != "" || reply.Message != "" {
		return reply, errors.New("protocol: invalid output reply")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return reply, fmt.Errorf("protocol: decode output fields: %w", err)
	}
	var resultFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["accessOutputResult"], &resultFields); err != nil {
		return reply, fmt.Errorf("protocol: decode output result fields: %w", err)
	}
	if len(resultFields) != 9 {
		return reply, errors.New("protocol: incomplete output result")
	}
	return reply, ValidateAccessOutputResult(*reply.Result, request)
}
