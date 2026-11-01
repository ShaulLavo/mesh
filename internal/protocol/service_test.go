package protocol

import (
	"reflect"
	"testing"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestServiceConversionClonesTrustedOrigins(t *testing.T) {
	origins := []string{"https://bun-demo.sprockt.dev"}
	info := ServiceInfo{Name: "api", Kind: "proxy", Target: "3000", PrivateHost: "api.mesh.test", TrustedOrigins: origins}
	service := ServiceFromInfo(info)
	origins[0] = "https://attacker.example"
	if service.TrustedOrigins[0] != "https://bun-demo.sprockt.dev" {
		t.Fatal("ServiceFromInfo retained caller trusted-origin storage")
	}
	converted := ServiceDefinitionInfo(service)
	service.TrustedOrigins[0] = "https://changed.example"
	if !reflect.DeepEqual(converted.TrustedOrigins, []string{"https://bun-demo.sprockt.dev"}) {
		t.Fatalf("ServiceDefinitionInfo retained service storage: %#v", converted.TrustedOrigins)
	}
}

func TestServiceInfoTrustedOriginsRoundTrip(t *testing.T) {
	service := meshserve.Service{Name: "api", Kind: meshserve.Proxy, Target: "3000", PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}
	if got := ServiceFromInfo(ServiceDefinitionInfo(service)); !got.Equal(service) {
		t.Fatalf("service conversion changed definition: %#v", got)
	}
}
