package serve

import (
	"strings"
	"testing"
)

func TestNormalizeRetainsExplicitPublicName(t *testing.T) {
	for _, name := range []string{"", "blog.shaulavo.dev", "my-blog.shaulavo.dev"} {
		t.Run(name, func(t *testing.T) {
			service, err := Normalize(Service{Name: "site", Kind: Proxy, Target: "3000", PublicName: name})
			if err != nil {
				t.Fatal(err)
			}
			if service.PublicName != name {
				t.Fatalf("public name = %q, want %q", service.PublicName, name)
			}
		})
	}
}

func TestNormalizeRejectsInvalidPublicName(t *testing.T) {
	invalid := []string{
		"shaulavo.dev",
		"api.blog.shaulavo.dev",
		"mesh.shaulavo.dev",
		"*.shaulavo.dev",
		"BLOG.shaulavo.dev",
		"blog.shaulavo.dev.",
		"shaulavo.dev.example.com",
		"notshaulavo.dev",
		"bad_.shaulavo.dev",
		"-bad.shaulavo.dev",
		"bad-.shaulavo.dev",
		"bad..shaulavo.dev",
		strings.Repeat("a", 64) + ".shaulavo.dev",
	}
	for _, name := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := Normalize(Service{Name: "site", Kind: Proxy, Target: "3000", PublicName: name}); err == nil {
				t.Fatalf("invalid public name %q succeeded", name)
			}
		})
	}
}
