package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/shaul/mesh/internal/session"
)

// LineTail as a worker.line.attach LastSeq starts the attachment at the next
// stdout frame. The attach reply reports the actual starting frame in LastSeq
// and the next stdin offset in InputOffset.
const LineTail = ^uint64(0)

// TypeWorkerLineOversize reports one oversized JSON-RPC reply line whose
// delimiter and response ID were verified within the stdio drain cap, so
// framing remains intact for subsequent lines.
const TypeWorkerLineOversize = "worker.line.oversize"

// DecodeLineInput decodes one stdin frame from an exclusive worker connection.
// The first control binds the exact reservation and next output frame. After
// that, KindData carries stdout lines numbered from zero. KindInput carries
// the expected stdin byte offset in big endian, a one-byte key length, the
// key, and one exact newline-terminated line. Line write keys start with
// the decimal offset followed by a colon and a caller token. This makes an
// acknowledged key safe to retire and prevents reuse at a later offset.
// After recording an acknowledgment, the controller can send worker.line.retire
// with its write key. A retry of a retired write reports unknown acceptance.
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

// LineWriteOffset reads the immutable offset in a framed write key.
func LineWriteOffset(key string) (uint64, error) {
	if err := session.ValidateWriteKey(key); err != nil {
		return 0, fmt.Errorf("worker line: %w", err)
	}
	prefix, token, found := strings.Cut(key, ":")
	offset, err := strconv.ParseUint(prefix, 10, 64)
	if !found || token == "" || err != nil || strconv.FormatUint(offset, 10) != prefix {
		return 0, fmt.Errorf("worker line: write key must have a decimal offset prefix")
	}
	return offset, nil
}
