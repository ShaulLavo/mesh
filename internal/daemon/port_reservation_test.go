package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestRunKeepsReservedTCPListenersUntilBinding(t *testing.T) {
	controlPort := reserveTCPPort(t, "127.0.0.1")
	httpsPort := reserveTCPPort(t, "127.0.0.1")
	competitor, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", controlPort))
	if err == nil {
		t.Cleanup(func() { _ = competitor.Close() })
	}
	readyErr := errors.New("reserved listeners reached readiness")
	signerID, _ := composedIdentity(t)
	options := defaultRunOptions()
	options.discoverSelf = func(context.Context) (tailnet.Peer, error) {
		return tailnet.Peer{Addrs: []string{"127.0.0.1"}}, nil
	}
	options.validateServeAddresses = func([]string) error { return nil }
	options.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return nil, readyErr
	}
	err = run(t.Context(), Config{
		StateDir: t.TempDir(), TailnetPort: controlPort, HTTPSPort: httpsPort,
		CertificateRenewerID: signerID, TailscaleServe: true,
	}, options)
	if !errors.Is(err, readyErr) {
		t.Fatalf("reserved listener handoff = %v, want readiness error", err)
	}
}
