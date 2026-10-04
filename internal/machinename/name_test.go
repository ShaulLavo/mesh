package machinename

import (
	"strings"
	"testing"
)

func TestNormalizeDestinationNames(t *testing.T) {
	for input, want := range map[string]string{
		"  Office-PC  ": "office-pc", "pc": "pc", "a": "a", strings.Repeat("a", 63): strings.Repeat("a", 63),
	} {
		got, err := Normalize(input)
		if err != nil || got != want {
			t.Fatalf("normalization got %q, error %v", got, err)
		}
	}
	for _, input := range []string{"", " ", "-pc", "pc-", "pc.home", "pc home", "mác", "../pc", "7K3D", strings.Repeat("a", 64)} {
		if _, err := Normalize(input); err == nil {
			t.Fatalf("accepted invalid machine name %q", input)
		}
	}
	for command := range reservedNames {
		if _, err := Normalize(command); err == nil {
			t.Fatalf("accepted reserved command %q", command)
		}
	}
}

func TestInitialNameIsChosenAtDestination(t *testing.T) {
	if got := Initial("fixture", "Office-PC.example.test.", "different-os-host"); got != "office-pc" {
		t.Fatal(got)
	}
	if got := Initial("fixture", "ls", "fallback-host"); got != "fallback-host" {
		t.Fatal(got)
	}
	fallback := Initial("fixture", "", "7K3D")
	if _, err := Normalize(fallback); err != nil || fallback != Initial("fixture") {
		t.Fatalf("destination fallback is invalid: %v", err)
	}
}
