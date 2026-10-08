package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

const shadowTestWarning = "private route /blog/admin shadows public route https://blog.mesh.test/blog at /blog/admin; public requests there return 404"

func TestServePrintsRegistrationShadowWarning(t *testing.T) {
	host := setupCommandTestHost(t)
	service := protocol.ServiceInfo{Name: "blog/admin", Kind: "proxy", Target: "3000"}
	dial := serviceRemoteDial(host.host, func(request protocol.Control) protocol.Control {
		switch request.Type {
		case protocol.TypeServiceList:
			return protocol.Control{Type: protocol.TypeServiceListed}
		case protocol.TypeServicePreview:
			return protocol.Control{Type: protocol.TypeServicePreviewed, ServicePreview: &protocol.ServicePreview{Service: service}}
		case protocol.TypeServiceUpsert:
			return protocol.Control{Type: protocol.TypeServiceUpserted, Service: &service, Message: shadowTestWarning}
		default:
			t.Fatalf("unexpected request %s", request.Type)
			return protocol.Control{}
		}
	})
	stdout, stderr, err := executeCommand(t, Dependencies{DialControl: dial}, "serve", "pc", "3000", "--at", "/blog/admin")
	if err != nil || !strings.Contains(stdout, "serving ") {
		t.Fatalf("serve = %q, %v", stdout, err)
	}
	if !strings.Contains(stderr, "warning: pc: "+shadowTestWarning) {
		t.Fatalf("serve omitted the shadow warning: %q", stderr)
	}
}

func TestServiceShadowWarningsStayOnTheirOwnHost(t *testing.T) {
	public := ServiceCatalogRow{Host: HostRecord{ID: "pc", MachineName: "pc"}, Live: true,
		Service: protocol.ServiceInfo{Name: "blog", PublicName: "blog.mesh.test"}}
	private := ServiceCatalogRow{Host: HostRecord{ID: "pi", MachineName: "pi"}, Live: true,
		Service: protocol.ServiceInfo{Name: "blog/admin"}}
	var output bytes.Buffer
	if err := writeServiceShadowWarnings(&output, []ServiceCatalogRow{public, private}); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("routes on different hosts produced a warning: %q", output.String())
	}
	private.Host = public.Host
	public.Live, private.Live = false, false
	if err := writeServiceShadowWarnings(&output, []ServiceCatalogRow{public, private}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "warning: pc (cached): "+shadowTestWarning+"\n" {
		t.Fatalf("cached warning = %q", output.String())
	}
}

func TestServeListWarnsAboutPrivateRouteShadowing(t *testing.T) {
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{
		{Name: "blog", Kind: "proxy", Target: "3000", PublicName: "blog.mesh.test", Healthy: true},
		{Name: "blog/admin", Kind: "proxy", Target: "4000", Healthy: true},
	}
	for _, command := range []string{"ls", "list"} {
		stdout, stderr, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", command)
		if err != nil || !strings.Contains(stdout, "/blog/admin") {
			t.Fatalf("serve %s = %q, %v", command, stdout, err)
		}
		if !strings.Contains(stderr, "warning: pc: "+shadowTestWarning) {
			t.Fatalf("serve %s omitted the shadow warning: %q", command, stderr)
		}
	}
}
