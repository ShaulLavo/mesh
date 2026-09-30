package daemon

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/transport"
)

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
		if err != nil || !slices.Equal(normalized.proxyForwarderUIDs, []uint32{0, uint32(os.Getuid())}) {
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
