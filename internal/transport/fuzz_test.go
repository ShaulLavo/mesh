package transport

import (
	"bytes"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func FuzzProtocolReader(f *testing.F) {
	f.Add([]byte{byte(protocol.KindControl), 0, 0, 0, 2, '{', '}'})
	f.Add([]byte{byte(protocol.KindData), 0, 0, 0, 16,
		'7', 'K', '3', 'D', 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 1})
	f.Add([]byte{0x7f, 0, 0, 0, 0})
	f.Add([]byte{byte(protocol.KindData), 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, wire []byte) {
		frame, err := protocol.NewReader(bytes.NewReader(wire)).ReadFrame()
		if err != nil {
			return
		}
		if len(frame.Payload) > protocol.MaxPayload {
			t.Fatalf("decoded payload has %d bytes, maximum is %d", len(frame.Payload), protocol.MaxPayload)
		}
	})
}
