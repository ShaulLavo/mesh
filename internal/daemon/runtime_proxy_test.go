package daemon

import (
	"context"
	"crypto/tls"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/transport"
)

func proxyTestUID(t *testing.T) uint32 {
	t.Helper()
	uid := int64(os.Getuid())
	if uid >= 0 && uid <= math.MaxUint32 {
		return uint32(uid)
	}
	t.Fatalf("test process UID %d is outside the kernel UID range", uid)
	return 0
}

func TestTailnetOwnerAccessValidatesPeerUIDSupportAtStartup(t *testing.T) {
	cfg := ListenerConfig{
		StateDir: t.TempDir(), PublicListenAddress: "127.0.0.1:8445", TailnetOwnerAccess: true,
		PublicHTTPHandler: http.NotFoundHandler(),
		PublicTLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return nil, nil
		}},
	}
	handler := func(context.Context, transport.Conn) error { return nil }
	normalized, err := validateListenerConfig(context.Background(), cfg, handler)
	if runtime.GOOS == "linux" {
		if err != nil || !slices.Equal(normalized.proxyForwarderUIDs, []uint32{0, proxyTestUID(t)}) {
			t.Fatalf("owner access allow-list = %v, %v", normalized.proxyForwarderUIDs, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "enable Tailnet owner access") || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("unsupported owner access startup = %v", err)
	}
	cfg.TailnetOwnerAccess = false
	normalized, err = validateListenerConfig(context.Background(), cfg, handler)
	if err != nil || len(normalized.proxyForwarderUIDs) != 0 {
		t.Fatalf("manual browser pairing listener = %v, %v", normalized.proxyForwarderUIDs, err)
	}
}
