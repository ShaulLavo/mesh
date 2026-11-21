package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/shaul/mesh/internal/session"
)

var (
	ErrInputUnknown  = errors.New("stdin acceptance is unknown")
	ErrInputConflict = errors.New("write key already names different stdin data")
	ErrInputOffset   = errors.New("stdin offset does not match")
	ErrInputClosed   = errors.New("stdin is closed")
	ErrInputFull     = errors.New("stdin buffer is full")
	ErrInputLimit    = errors.New("stdin receipt limit reached")
)

type inputEvidence struct {
	Binding Binding              `json:"binding"`
	Digest  string               `json:"digest"`
	Receipt session.InputReceipt `json:"receipt"`
}

func inputPath(b Binding, key string) string {
	return attempt(b, filepath.Join("input", digest(key)+".json"))
}

func (a *Attempt) RecordInput(receipt session.InputReceipt, dataDigest string) (Stage, error) {
	name := inputPath(a.binding, receipt.WriteKey)
	record := inputEvidence{Binding: a.binding, Digest: dataDigest, Receipt: receipt}
	stage, err := a.files.Publish(name, record, true)
	if err != nil {
		return stage, fmt.Errorf("%w: publish stdin receipt: %w", ErrInputUnknown, err)
	}
	return stage, nil
}

func (a *Attempt) PrepareInput() error {
	return a.files.Ensure(attempt(a.binding, "input"))
}

func InputDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Input recovers a durable acceptance even when the worker has retired. A
// missing record after loss of the worker is not proof of non-acceptance.
func (o *Output) Input(b Binding, key string, expected *session.InputReceipt, data []byte) (*session.InputReceipt, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if err := session.ValidateWriteKey(key); err != nil {
		return nil, fmt.Errorf("validate stdin write key: %w", err)
	}
	name := inputPath(b, key)
	var evidence inputEvidence
	if err := o.files.Read(name, &evidence); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrInputUnknown
	} else if err != nil {
		return nil, fmt.Errorf("%w: read stdin receipt: %w", ErrInputUnknown, err)
	}
	if evidence.Binding != b || evidence.Receipt.WriteKey != key || !validDigest(evidence.Digest) || evidence.Receipt.Length < 0 || evidence.Receipt.Length > session.MaxLineBytes {
		return nil, fmt.Errorf("%w: invalid stdin receipt", ErrInputUnknown)
	}
	if expected != nil && (evidence.Receipt != *expected || evidence.Digest != InputDigest(data)) {
		return nil, ErrInputConflict
	}
	if err := o.files.Confirm(name); err != nil {
		return nil, fmt.Errorf("%w: confirm stdin receipt: %w", ErrInputUnknown, err)
	}
	return &evidence.Receipt, nil
}

func InputErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrInputUnknown):
		return "input.outcome_unknown"
	case errors.Is(err, ErrInputConflict):
		return "input.key_conflict"
	case errors.Is(err, ErrInputOffset):
		return "input.offset_mismatch"
	case errors.Is(err, ErrInputClosed):
		return "input.closed"
	case errors.Is(err, ErrInputFull):
		return "input.backpressure"
	case errors.Is(err, ErrInputLimit):
		return "resource.limit"
	default:
		return ""
	}
}
