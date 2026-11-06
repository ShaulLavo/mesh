// Package agentapi defines the finite agent command results independently of Mesh storage and transport.
package agentapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const SchemaVersion = 1

type Status string

const (
	StatusOK      Status = "ok"
	StatusPartial Status = "partial"
	StatusError   Status = "error"
)

type Envelope struct {
	SchemaVersion int     `json:"schemaVersion"`
	Operation     string  `json:"operation"`
	Status        Status  `json:"status"`
	RequestID     string  `json:"requestId"`
	Data          any     `json:"data"`
	Errors        []Error `json:"errors"`
}

type Error struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Operation string `json:"operation"`
	Target    string `json:"target,omitempty"`
	Retryable bool   `json:"retryable"`
}

type Failure struct {
	Err  Error
	Exit int
}

func (f *Failure) Error() string { return f.Err.Message }

func Fail(code Code, operation, target, message string) *Failure {
	class, ok := Codes[code]
	if !ok || class.Reserved {
		code, class = InternalError, Codes[InternalError]
	}
	return &Failure{Err: Error{Code: code, Message: CleanMessage(message), Operation: operation, Target: target, Retryable: class.Retryable}, Exit: class.Exit}
}

func Usage(operation, message string) *Failure { return Fail(UsageInvalid, operation, "", message) }

func CleanMessage(message string) string {
	message = strings.ToValidUTF8(message, "?")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, message)
	if len(message) <= 512 {
		return message
	}
	limit := 512
	for !utf8.ValidString(message[:limit]) {
		limit--
	}
	return message[:limit]
}

func Write(w io.Writer, envelope Envelope) error {
	if envelope.Errors == nil {
		envelope.Errors = []Error{}
	}
	for i := range envelope.Errors {
		envelope.Errors[i].Message = CleanMessage(envelope.Errors[i].Message)
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return fmt.Errorf("write agent result: %w", err)
	}
	return nil
}

func (e Envelope) ExitCode() int {
	switch e.Status {
	case StatusOK:
		return 0
	case StatusPartial:
		return 3
	case StatusError:
		if len(e.Errors) > 0 && Codes[e.Errors[0].Code].Exit == 2 {
			return 2
		}
		return 1
	}
	return 1
}

func Requested(args []string) bool {
	requested := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			requested = true
		}
		if value, ok := strings.CutPrefix(arg, "--json="); ok {
			requested = requestedBoolean(value, requested)
		}
	}
	return requested
}

func requestedBoolean(value string, previous bool) bool {
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return previous
	}
	return parsed
}

// NewRequestID produces a 26-character Crockford ID, with a millisecond prefix.
func NewRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("agent request ID: %w", err)
	}
	millis := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		raw[i], millis = byte(millis), millis>>8
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var encoded [26]byte
	for i := 25; i >= 0; i-- {
		encoded[i] = alphabet[raw[15]&31]
		shiftRequestBits(&raw)
	}
	return string(encoded[:]), nil
}

func shiftRequestBits(raw *[16]byte) {
	for i := 15; i > 0; i-- {
		raw[i] = raw[i]>>5 | raw[i-1]<<3
	}
	raw[0] >>= 5
}
