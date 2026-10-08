package serve

import "testing"

func TestConfiguredPublicDomainOverlap(t *testing.T) {
	for _, name := range []string{"blog.mesh.test", "blog.old.test"} {
		if err := ValidatePublicName(name); err != nil {
			t.Fatalf("accepted domain %s: %v", name, err)
		}
	}
	for _, name := range []string{"blog.other.test", "nested.blog.old.test", "mesh.old.test"} {
		if err := ValidatePublicName(name); err == nil {
			t.Fatalf("unconfigured or reserved name accepted: %s", name)
		}
	}
}
