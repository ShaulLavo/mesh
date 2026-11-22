package mcpnative

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

const (
	minTimeoutMS      = 100
	maxHostListMS     = 30000
	maxTimeoutMS      = 60000
	defaultHostMS     = 2000
	defaultOpMS       = 5000
	defaultWaitMS     = 10000
	defaultPageLimit  = 100
	maxPageLimit      = 500
	defaultOutputMax  = 65536
	maxOutputBytes    = 1 << 20
	defaultExecOutput = 16384
	maxExecOutput     = 256 << 10
	maxCursorLength   = 2048
)

type paramError struct {
	message string
}

func (e *paramError) Error() string { return e.message }

func invalidParam(format string, args ...any) error {
	return &paramError{message: fmt.Sprintf(format, args...)}
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalidParam("invalid tool arguments: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalidParam("invalid tool arguments: trailing JSON")
	}
	return nil
}

func parseTimeout(ms *int, defaultMS, maxMS int) (time.Duration, error) {
	if ms == nil {
		return time.Duration(defaultMS) * time.Millisecond, nil
	}
	if *ms < minTimeoutMS || *ms > maxMS {
		return 0, invalidParam("timeout_ms must be between %d and %d", minTimeoutMS, maxMS)
	}
	return time.Duration(*ms) * time.Millisecond, nil
}

func parseWaitMS(ms *int) (int, error) {
	if ms == nil {
		return defaultWaitMS, nil
	}
	if *ms < minTimeoutMS || *ms > maxTimeoutMS {
		return 0, invalidParam("wait_ms must be between %d and %d", minTimeoutMS, maxTimeoutMS)
	}
	return *ms, nil
}

func requireNonEmpty(value, name string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", invalidParam("%s is required", name)
	}
	return trimmed, nil
}

func parseLimit(limit *int) (int, error) {
	if limit == nil {
		return defaultPageLimit, nil
	}
	if *limit < 1 || *limit > maxPageLimit {
		return 0, invalidParam("limit must be between 1 and %d", maxPageLimit)
	}
	return *limit, nil
}

func parseOutputStream(stream string) (string, error) {
	if stream == "" {
		return execution.StreamStdout, nil
	}
	switch stream {
	case execution.StreamStdout, execution.StreamStderr, execution.KindPTY:
		return stream, nil
	default:
		return "", invalidParam("stream must be stdout, stderr, or pty")
	}
}

func parseMaxBytes(value *int) (int, error) {
	if value == nil {
		return defaultOutputMax, nil
	}
	if *value < 1 || *value > maxOutputBytes {
		return 0, invalidParam("max_bytes must be between 1 and %d", maxOutputBytes)
	}
	return *value, nil
}

func parseInputPayload(offsetText *string, text, b64 *string, eof bool) (*uint64, []byte, error) {
	if text != nil && b64 != nil {
		return nil, nil, invalidParam("data and data_base64 are mutually exclusive")
	}
	if offsetText == nil {
		if text != nil || b64 != nil || eof {
			return nil, nil, invalidParam("offset is required when writing data or closing stdin with eof")
		}
		return nil, nil, nil
	}
	offset, err := strconv.ParseUint(*offsetText, 10, 64)
	if err != nil || strconv.FormatUint(offset, 10) != *offsetText {
		return nil, nil, invalidParam("offset must be a nonnegative decimal string")
	}
	data, err := decodeInputData(text, b64)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > session.MaxInputBytes {
		return nil, nil, invalidParam("input payload exceeds %d bytes", session.MaxInputBytes)
	}
	if len(data) == 0 && !eof {
		return nil, nil, invalidParam("provide data, data_base64, or eof=true when offset is set")
	}
	if offset > ^uint64(0)-uint64(len(data)) {
		return nil, nil, invalidParam("offset plus input length overflows uint64")
	}
	return &offset, data, nil
}

func decodeInputData(text, b64 *string) ([]byte, error) {
	if b64 != nil {
		decoded, err := base64.StdEncoding.Strict().DecodeString(*b64)
		if err != nil {
			return nil, invalidParam("data_base64 is not valid standard base64")
		}
		return decoded, nil
	}
	if text != nil {
		return []byte(*text), nil
	}
	return nil, nil
}

func validateSignalName(sig string) (string, error) {
	if !worker.SupportsSignal(sig) {
		return "", invalidParam("unsupported signal %q", sig)
	}
	return sig, nil
}

func validateRequestKey(key string) error {
	if err := (execution.Key{Host: "validation", Class: "local-account", Principal: "account", Opaque: key}).Validate(); err != nil {
		return invalidParam("invalid request_key: %v", err)
	}
	return nil
}

func validateWriteKey(key string) error {
	if err := session.ValidateWriteKey(key); err != nil {
		return invalidParam("invalid write_key: %v", err)
	}
	return nil
}

func parseOptionalContext(raw string) (*ContextRef, error) {
	if raw == "" {
		return nil, nil
	}
	ref, err := ParseContextHandle(raw)
	if err != nil {
		return nil, invalidParam("%v", err)
	}
	return &ref, nil
}

func parseRevision(raw json.RawMessage, required bool) (*uint64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		if required {
			return nil, invalidParam("expected_revision is required")
		}
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, invalidParam("expected_revision must be a nonnegative integer or decimal string")
		}
		n, err := strconv.ParseUint(text, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != text {
			return nil, invalidParam("expected_revision must be a canonical nonnegative decimal string")
		}
		return &n, nil
	}
	var n uint64
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return nil, invalidParam("expected_revision must be a nonnegative integer or decimal string")
	}
	return &n, nil
}

func parseContextShell(shell string) (string, error) {
	if shell == "" {
		return protocol.ContextShellLogin, nil
	}
	switch shell {
	case protocol.ContextShellLogin, protocol.ContextShellSh, protocol.ContextShellNone:
		return shell, nil
	default:
		return "", invalidParam("shell must be login, sh, or none")
	}
}

func parseExecStdin(stdin string) (string, error) {
	if stdin == "" {
		return execution.StdinClosed, nil
	}
	switch stdin {
	case execution.StdinClosed, execution.StdinOpen:
		return stdin, nil
	default:
		return "", invalidParam("stdin must be closed or open")
	}
}

func parseExecWaitMS(ms *int) (int, error) {
	if ms == nil {
		return defaultWaitMS, nil
	}
	if *ms < 0 || *ms > maxTimeoutMS {
		return 0, invalidParam("wait_ms must be between 0 and %d", maxTimeoutMS)
	}
	return *ms, nil
}

func parseExecOutputBytes(value *int) (int, error) {
	if value == nil {
		return defaultExecOutput, nil
	}
	if *value < 1 || *value > maxExecOutput {
		return 0, invalidParam("output_bytes must be between 1 and %d", maxExecOutput)
	}
	return *value, nil
}

func parseExecCommand(command *string, argv []string) (string, []string, error) {
	hasCmd := command != nil && *command != ""
	hasArgv := len(argv) > 0
	if hasCmd == hasArgv {
		return "", nil, invalidParam("pass exactly one of command or argv")
	}
	if hasCmd {
		return *command, nil, nil
	}
	if len(argv) > 1024 {
		return "", nil, invalidParam("argv exceeds 1024 entries")
	}
	if argv[0] == "" {
		return "", nil, invalidParam("argv[0] must be non-empty")
	}
	return "", argv, nil
}

func parseEnvMap(env map[string]string, field string) error {
	if len(env) > protocol.MaxContextEnvVars {
		return invalidParam("%s exceeds %d variables", field, protocol.MaxContextEnvVars)
	}
	size := 0
	for name, val := range env {
		if err := validateEnvName(name, field); err != nil {
			return err
		}
		if strings.ContainsRune(val, 0) {
			return invalidParam("%s[%q] must not contain NUL bytes", field, name)
		}
		size += len(name) + len(val) + 2
	}
	if size > protocol.MaxContextEnvBytes {
		return invalidParam("%s exceeds %d bytes", field, protocol.MaxContextEnvBytes)
	}
	return nil
}

func validateEnvName(name, field string) error {
	if name == "" || strings.HasPrefix(name, "MESH_") {
		return invalidParam("%s name %q is empty or reserved", field, name)
	}
	for i, ch := range name {
		if ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (i > 0 && ch >= '0' && ch <= '9') {
			continue
		}
		return invalidParam("%s name %q must match [A-Za-z_][A-Za-z0-9_]*", field, name)
	}
	return nil
}
