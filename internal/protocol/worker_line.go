package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/shaul/mesh/internal/session"
)

// DecodeLineInput decodes one stdin frame from an exclusive worker connection.
// The first control binds the exact reservation and next output frame. After
// that, KindData carries stdout lines numbered from zero. KindInput carries
// the expected stdin byte offset in big endian, a one-byte key length, the
// key, and one exact newline-terminated line.
func DecodeLineInput(payload []byte) (session.InputReceipt, []byte, error) {
	if len(payload) < 9 || len(payload) < 9+int(payload[8]) {
		return session.InputReceipt{}, nil, fmt.Errorf("worker line: invalid input header")
	}
	key := string(payload[9 : 9+int(payload[8])])
	line := payload[9+int(payload[8]):]
	if err := validateLineInput(key, line); err != nil {
		return session.InputReceipt{}, nil, err
	}
	offset := binary.BigEndian.Uint64(payload)
	if offset > ^uint64(0)-uint64(len(line)) {
		return session.InputReceipt{}, nil, fmt.Errorf("worker line: invalid offset")
	}
	return session.InputReceipt{WriteKey: key, Offset: offset, Length: len(line)}, line, nil
}

func validateLineInput(key string, line []byte) error {
	if err := session.ValidateWriteKey(key); err != nil {
		return fmt.Errorf("worker line: %w", err)
	}
	if len(line) == 0 || len(line) > session.MaxLineBytes || bytes.IndexByte(line, '\n') != len(line)-1 {
		return fmt.Errorf("worker line: input must contain one complete line")
	}
	return nil
}
