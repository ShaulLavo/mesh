package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestRunKeepsReservedTCPListenersUntilBinding(t *testing.T) {
	controlListener, controlPort := newTCPListener(t, "127.0.0.1:0")
	httpsListener, httpsPort := newTCPListener(t, "127.0.0.1:0")
	for _, port := range []uint16{controlPort, httpsPort} {
		competitor, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			t.Cleanup(func() { _ = competitor.Close() })
			t.Fatalf("reserved port %d was available to a competing listener", port)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("competing bind on reserved port %d = %v, want address in use", port, err)
		}
	}
	readyErr := errors.New("reserved listeners reached readiness")
	signerID, _ := composedIdentity(t)
	options := defaultRunOptions()
	options.listen = useTCPListeners(controlListener, httpsListener)
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return nil, readyErr
	}
	err := run(t.Context(), Config{
		StateDir: t.TempDir(), TailnetPort: controlPort, HTTPSPort: httpsPort,
		CertificateRenewerID: signerID, TailscaleServe: true,
	}, options)
	if !errors.Is(err, readyErr) {
		t.Fatalf("reserved listener handoff = %v, want readiness error", err)
	}
	for _, listener := range []net.Listener{controlListener, httpsListener} {
		tcp := listener.(*net.TCPListener)
		_ = tcp.SetDeadline(time.Now())
		if _, err := tcp.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("reserved listener after readiness failure = %v, want closed", err)
		}
	}
}
