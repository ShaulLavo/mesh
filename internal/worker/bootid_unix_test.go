//go:build linux || darwin

package worker

import "testing"

func TestBootIdentityIsPresentAndStable(t *testing.T) {
	first := BootID()
	if first == "" {
		t.Fatal("running kernel did not expose its boot identity")
	}
	if next := BootID(); next != first {
		t.Fatalf("boot identity changed without reboot: %q then %q", first, next)
	}
}
