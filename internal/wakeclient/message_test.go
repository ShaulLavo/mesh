package wakeclient

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A wake sender's error text reaches the user's terminal, so an oversized one
// is cut on a character boundary; a short one is unchanged.
func TestBoundedRemoteMessage(t *testing.T) {
	if got := boundedRemoteMessage("sender busy"); got != "sender busy" {
		t.Fatalf("short message = %q", got)
	}
	// The leading ASCII byte puts the cut inside a two-byte "é", so the cut
	// has to back up to the start of that character.
	long := "a" + strings.Repeat("é", maxRemoteMessage)
	got := boundedRemoteMessage(long)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > maxRemoteMessage+len("…") {
		t.Fatalf("long message cut to %d bytes, valid %t", len(got), utf8.ValidString(got))
	}
}
