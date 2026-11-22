package cli

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// Output reads can wait on a busy stream for their whole deadline. With as
// many of them in flight as there are short-control slots, a short control
// such as a signal must still reach the host (PLAN section 2.5), and each read
// must tell the host how long it may wait.
func TestMCPOutputReadsLeaveShortControlSlotsFree(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(dir, "state"))
	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	inst := "11111111111111111111111111111111"
	host := HostRecord{ID: hostID, MeshIdentity: hostID, MachineName: "pc", Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan int, mcpPoolShortLimit)
	pool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
			case protocol.TypeExecutionLogs:
				entered <- req.TimeoutMillis
				<-release
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionPage: &protocol.ExecutionPage{
					HostID: h.ID, SessionID: req.SessionID, Instance: req.Instance, Stream: req.Stream,
					ObservedEnd: "0", Generation: "1", Available: true,
				}}, nil
			default:
				return protocol.Control{Type: protocol.TypeExecutionReceipt}, nil
			}
		}), nil
	}, time.Now)
	defer pool.Close()
	controls := newMCPControls(&application{dependencies: Dependencies{Now: time.Now}}, pool, "mac1_test_cred")

	var reads sync.WaitGroup
	defer func() { close(release); reads.Wait() }()
	for range mcpPoolShortLimit {
		reads.Go(func() {
			_, _ = controls.JobOutput(t.Context(), mcpnative.JobOutputRequest{
				Job: mcpnative.JobRef{HostID: hostID, SessionID: "7K3D", Instance: inst}, Stream: "stdout", MaxBytes: 64, Timeout: 30 * time.Second,
			})
		})
	}
	bucket, err := pool.bucket(host)
	if err != nil {
		t.Fatal(err)
	}
	// Every read is either waiting at the host or queued behind the full
	// long-stream slots before the short control is sent.
	waiting := 0
	for hang := time.After(outputSlotsHangCap); waiting < mcpPoolShortLimit && !(waiting == mcpPoolStreamLimit && len(bucket.streamSlots) == mcpPoolStreamLimit); {
		select {
		case wait := <-entered:
			if wait < 1 || wait > 30000 {
				t.Fatalf("output read told the host to wait %d ms, want its remaining 30 s budget", wait)
			}
			waiting++
		case <-time.After(time.Millisecond):
		case <-hang:
			t.Fatalf("only %d output reads reached the host", waiting)
		}
	}
	short, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	signal := protocol.Control{Type: protocol.TypeExecutionSignal, SessionID: "7K3D", Instance: inst, Signal: "TERM"}
	if _, _, err := pool.ExchangeShort(short, host, signal, false); err != nil {
		t.Fatalf("a short control could not get through while output reads waited: %v", err)
	}
}

// outputSlotsHangCap only catches a stuck test.
const outputSlotsHangCap = 30 * time.Second
