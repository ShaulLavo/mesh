package domainpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParsePolicy(t *testing.T) {
	p, err := Parse(strings.NewReader(`{"primary":"new.example","aliases":["old.example"],"legacyCertificateDomain":"old.example"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"pc.mesh.new.example", "pc.mesh.old.example"} {
		label, _, ok := p.Label(host, true)
		if !ok || label != "pc" {
			t.Fatalf("private alias %s", host)
		}
	}
	for _, host := range []string{"pc.mesh.evil.example", "a.pc.mesh.old.example", "pc.mesh.old.example.evil"} {
		if _, _, ok := p.Label(host, true); ok {
			t.Fatalf("accepted %s", host)
		}
	}
}

func TestRejectInvalidPolicy(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"primary":"EXAMPLE.test"}`, `{"primary":"*.example.test"}`, `{"primary":"https://example.test"}`,
		`{"primary":"127.0.0.1"}`, `{"primary":"example..test"}`, `{"primary":"example.test."}`,
		`{"primary":"example.test","aliases":["example.test"]}`, `{"primary":"example.test","aliases":["sub.example.test"]}`,
		`{"primary":"example.test","legacyCertificateDomain":"other.test"}`, `{"primary":"example.test","extra":true}`,
		`{"primary":"example.test"} {}`,
	} {
		if _, err := Parse(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestPolicyRejectsOversizedInput(t *testing.T) {
	if _, err := Parse(strings.NewReader(strings.Repeat(" ", 65537))); err == nil {
		t.Fatal("oversized policy accepted")
	}
}

func TestInitializeIsImmutableAndOptional(t *testing.T) {
	t.Cleanup(func() { active = Policy{}; initialize = sync.Once{} })
	path := filepath.Join(t.TempDir(), "domains.json")
	if err := Initialize(path); err != nil {
		t.Fatal(err)
	}
	if Primary() != "" {
		t.Fatal("missing configuration enabled names")
	}
	if err := os.WriteFile(path, []byte(`{"primary":"new.example","aliases":["old.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(path); err != nil {
		t.Fatal(err)
	}
	policyCopy := Current()
	policyCopy.Aliases[0] = "foreign.example"
	if Domains()[1] != "old.example" {
		t.Fatal("policy exposed mutable aliases")
	}
	if err := Initialize(path); err == nil {
		t.Fatal("policy reinitialized")
	}
}

func TestMissingPolicyAcceptsNoNames(t *testing.T) {
	var policy Policy
	if len(policy.Domains()) != 0 {
		t.Fatal("missing policy exposes a domain")
	}
	for _, private := range []bool{false, true} {
		for _, host := range []string{"app.", "pc.mesh.", "pc.mesh.new.example"} {
			if _, _, accepted := policy.Label(host, private); accepted {
				t.Fatalf("missing policy accepted %s", host)
			}
		}
	}
}
