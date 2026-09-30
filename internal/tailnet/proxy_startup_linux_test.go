package tailnet

import (
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProxyForwarderUIDsRejectsMissingDiagnosticHandler(t *testing.T) {
	uids, err := proxyForwarderUIDs(peerUIDFunc(func(net.Conn) (uint32, error) {
		return 0, unix.ENOENT
	}))
	if err == nil || uids != nil || !errors.Is(err, unix.ENOENT) || !strings.Contains(err.Error(), "PROXY forwarder authentication unavailable") {
		t.Fatalf("missing diagnostic handler enabled owner access: UIDs %v, error %v", uids, err)
	}
}
