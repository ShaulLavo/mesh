package serve

import "testing"

func TestServiceDisplayNameValidation(t *testing.T) {
	for _, name := range []string{"", "CLI Proxy", "Comfy Gallery", "גלריה"} {
		if err := ValidateDisplayName(name); err != nil {
			t.Fatalf("valid name %q: %v", name, err)
		}
	}
	for _, name := range []string{" leading", "trailing ", "one\ntwo", "tab\tname", "escape\x1b[31m", string([]byte{0xff})} {
		if err := ValidateDisplayName(name); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
}
