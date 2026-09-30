package tailnet

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProxyForwarderUIDsRequiresOwnedSocket(t *testing.T) {
	uid := proxyTestUID(t)
	for _, tt := range []struct {
		name  string
		owner uint32
		err   error
		ok    bool
	}{
		{name: "Mesh UID", owner: uid, ok: true},
		{name: "wrong UID", owner: uid + 1},
		{name: "missing handler", err: unix.ENOENT},
		{name: "blocked lookup", err: unix.EPERM},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var probe net.Conn
			uids, err := proxyForwarderUIDs(peerUIDFunc(func(c net.Conn) (uint32, error) {
				probe = c
				for _, endpoint := range []net.Addr{c.LocalAddr(), c.RemoteAddr()} {
					address, err := netip.ParseAddrPort(endpoint.String())
					if err != nil || !address.Addr().IsLoopback() || address.Port() == 0 {
						t.Fatalf("startup must inspect a live loopback connection: %s, %v", endpoint, err)
					}
				}
				actual, err := (systemPeerUIDLookup{}).PeerUID(c)
				if err != nil || actual != uid {
					t.Fatalf("startup probe's real owner = %d, %v; want %d", actual, err, uid)
				}
				return tt.owner, tt.err
			}))
			if probe == nil {
				t.Fatal("startup did not inspect its owned socket")
			}
			if _, closed := probe.Write([]byte("closed")); !errors.Is(closed, net.ErrClosed) {
				t.Fatalf("startup left probe connection open: %v", closed)
			}
			if tt.ok {
				if err != nil || !slices.Equal(uids, []uint32{0, uid}) {
					t.Fatalf("healthy startup = %v, %v", uids, err)
				}
			} else if err == nil || uids != nil || !strings.Contains(err.Error(), "PROXY forwarder authentication unavailable") ||
				tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("unverified probe enabled owner access: UIDs %v, error %v", uids, err)
			}
		})
	}
}

func TestProxyForwarderUIDsRejectsMissingDiagnosticHandler(t *testing.T) {
	uids, err := proxyForwarderUIDs(peerUIDFunc(func(net.Conn) (uint32, error) {
		return 0, unix.ENOENT
	}))
	if err == nil || uids != nil || !errors.Is(err, unix.ENOENT) || !strings.Contains(err.Error(), "PROXY forwarder authentication unavailable") {
		t.Fatalf("missing diagnostic handler enabled owner access: UIDs %v, error %v", uids, err)
	}
}
