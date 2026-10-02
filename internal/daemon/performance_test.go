package daemon

import (
	"context"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func BenchmarkRelayOutputQueue32K(b *testing.B) {
	relay := &clientRelay{lifetime: context.Background(), output: make(chan relayOutput, relayOutputQueueFrameLimit+relayOutputControlReserve)}
	frame := protocol.Frame{Kind: protocol.KindData, Payload: make([]byte, 32<<10)}
	b.SetBytes(int64(len(frame.Payload)))
	b.ReportAllocs()
	for b.Loop() {
		if err := relay.enqueueOutput(frame); err != nil {
			b.Fatal(err)
		}
		relay.releaseOutput((<-relay.output).frame)
	}
}
