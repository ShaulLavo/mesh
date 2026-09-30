package tailnet

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
)

func TestNetworkOwnersMatchAccountAndExcludeTaggedDevices(t *testing.T) {
	status := []byte(`{"BackendState":"Running","Self":{"DNSName":"origin.example.ts.net.","UserID":42,"TailscaleIPs":["100.64.0.1"]},"Peer":{"phone":{"UserID":42,"TailscaleIPs":["100.64.0.2","fd7a:115c:a1e0::2"]},"other":{"DNSName":"other.example.ts.net.","UserID":99,"TailscaleIPs":["100.64.0.3"]},"tagged":{"UserID":42,"Tags":["tag:server"],"TailscaleIPs":["100.64.0.4"]},"tagged-origin":{"DNSName":"tagged.example.ts.net.","UserID":42,"Tags":["tag:server"],"TailscaleIPs":["100.64.0.5"]},"no-user":{"TailscaleIPs":["100.64.0.6"]}}}`)
	calls := 0
	resolver := NewClient(runFunc(func(context.Context, string, ...string) ([]byte, []byte, error) {
		calls++
		return status, nil, nil
	})).OwnerResolver(map[string]string{"origin.example.ts.net": "owner", "other.example.ts.net": "other-owner", "tagged.example.ts.net": "tagged-owner"})
	for _, test := range []struct {
		ip     string
		owners []string
	}{
		{"100.64.0.1", []string{"owner"}}, {"100.64.0.2", []string{"owner"}}, {"fd7a:115c:a1e0::2", []string{"owner"}},
		{"100.64.0.3", []string{"other-owner"}}, {"100.64.0.4", nil}, {"100.64.0.6", nil}, {"203.0.113.1", nil}, {"127.0.0.1", nil},
	} {
		owners, err := resolver(context.Background(), netip.MustParseAddr(test.ip))
		if err != nil || !reflect.DeepEqual(owners, test.owners) {
			t.Fatalf("owners for %s = %v, %v", test.ip, owners, err)
		}
	}
	if calls != 1 {
		t.Fatalf("discovery ran %d times instead of using the bounded cache", calls)
	}
}
