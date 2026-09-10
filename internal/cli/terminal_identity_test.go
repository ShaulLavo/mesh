package cli

import (
	"os"
	"testing"

	"github.com/shaul/mesh/internal/worker"
)

// clearTerminalEnvironment removes every identifier a real terminal might have
// exported, so a test sees only what it sets.
func clearTerminalEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv(worker.MeshSessionIDVariable, "")
	_ = os.Unsetenv(worker.MeshSessionIDVariable)
	for _, probe := range terminalProbes {
		for _, name := range probe.variables {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
}

// The order matters more than any single probe: a pane identifier a terminal
// persists outlives one derived from a pointer or a device number.
func TestTerminalIdentityPrefersTheMostDurableIdentifier(t *testing.T) {
	clearTerminalEnvironment(t)
	t.Setenv("GHOSTTY_SURFACE_ID", "0x5767ab146f09a5d4")
	ghostty, ok := DiscoverTerminal()
	if !ok || ghostty.Source != "ghostty" {
		t.Fatalf("source = %q ok=%t, want ghostty", ghostty.Source, ok)
	}
	t.Setenv("CMUX_TAB_ID", "A22742C2-A89E-4390-BDB7-718AB21AC00A")
	tab, ok := DiscoverTerminal()
	if !ok || tab.Source != "cmux-tab" {
		t.Fatalf("source = %q ok=%t, want cmux-tab to outrank ghostty", tab.Source, ok)
	}
	t.Setenv("CMUX_SURFACE_ID", "252D854C-F7F8-48A0-A138-DD063DEB5528")
	pane, ok := DiscoverTerminal()
	if !ok || pane.Source != "cmux-pane" {
		t.Fatalf("source = %q ok=%t, want cmux-pane to outrank cmux-tab", pane.Source, ok)
	}
	t.Setenv(MeshTerminalIDVariable, "chosen-by-hand")
	explicit, ok := DiscoverTerminal()
	if !ok || explicit.Source != "mesh" {
		t.Fatalf("source = %q ok=%t, want an explicit MESH_TERMINAL_ID to outrank every probe", explicit.Source, ok)
	}
	for _, pair := range [][2]TerminalIdentity{{ghostty, tab}, {tab, pane}, {pane, explicit}} {
		if pair[0].Key == pair[1].Key {
			t.Fatalf("distinct identifiers produced the same key %q", pair[0].Key)
		}
	}
}

// The key has to be reproducible or a tab loses its session on every command.
func TestTerminalIdentityIsStableForTheSameTerminal(t *testing.T) {
	clearTerminalEnvironment(t)
	t.Setenv("CMUX_SURFACE_ID", "252D854C-F7F8-48A0-A138-DD063DEB5528")
	first, ok := DiscoverTerminal()
	if !ok {
		t.Fatal("terminal was not identified")
	}
	second, _ := DiscoverTerminal()
	if first.Key != second.Key {
		t.Fatalf("key changed between calls: %q then %q", first.Key, second.Key)
	}
	t.Setenv("CMUX_SURFACE_ID", "00000000-0000-0000-0000-000000000000")
	other, _ := DiscoverTerminal()
	if other.Key == first.Key {
		t.Fatal("a different pane produced the same key")
	}
}

// A client inside a session would otherwise rebind the outer tab to whatever it
// opened, silently stealing the tab from the session the user is sitting in.
func TestTerminalIdentityRefusesInsideAMeshSession(t *testing.T) {
	clearTerminalEnvironment(t)
	t.Setenv("CMUX_SURFACE_ID", "252D854C-F7F8-48A0-A138-DD063DEB5528")
	if _, ok := DiscoverTerminal(); !ok {
		t.Fatal("expected the terminal to be identified outside a session")
	}
	t.Setenv(worker.MeshSessionIDVariable, "7K3D")
	if identity, ok := DiscoverTerminal(); ok {
		t.Fatalf("identified %q inside a Mesh session; a nested client must not claim the outer tab", identity.Source)
	}
}
