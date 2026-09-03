package bootstrap

import (
	"testing"

	"github.com/shaul/mesh/internal/tailnet"
)

func TestPeerAnswersToTheNameAnOperatorTypes(t *testing.T) {
	t.Parallel()

	peer := tailnet.Peer{Name: "shauls-macbook-air.tail183c90.ts.net"}
	for _, typed := range []string{
		"shauls-macbook-air",
		"shauls-macbook-air.tail183c90.ts.net",
		"shauls-macbook-air.tail183c90.ts.net.",
		"SHAULS-MACBOOK-AIR",
	} {
		if !peerAnswersTo(peer, []string{typed, ""}) {
			t.Fatalf("peer %q does not answer to %q", peer.Name, typed)
		}
	}
}

func TestPeerAnswersToRejectsAnotherMachine(t *testing.T) {
	t.Parallel()

	peer := tailnet.Peer{Name: "shauls-macbook-air.tail183c90.ts.net"}
	// A short name must match the whole first label. Adopting the wrong machine
	// because a prefix matched would record one host under another name.
	for _, typed := range []string{
		"shauls-macbook",
		"macbook-air",
		"air",
		"shauls-macbook-air-2",
		"tail183c90",
		"",
		".",
	} {
		if peerAnswersTo(peer, []string{typed}) {
			t.Fatalf("peer %q wrongly answers to %q", peer.Name, typed)
		}
	}
}

func TestPeerAnswersToMatchesTheAliasWhenTheHostDiffers(t *testing.T) {
	t.Parallel()

	// ssh_config aliases mean the typed name and the resolved host are often
	// different, and either one may be the tailnet name.
	peer := tailnet.Peer{Name: "shauls-macbook-air.tail183c90.ts.net"}
	if !peerAnswersTo(peer, []string{"10.0.0.4", "shauls-macbook-air"}) {
		t.Fatal("peer does not answer to its alias when the host is an address")
	}
	if peerAnswersTo(peer, []string{"10.0.0.4", "someone-else"}) {
		t.Fatal("peer answers to an unrelated alias")
	}
}
