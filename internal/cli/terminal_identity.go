package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/worker"
)

// MeshTerminalIDVariable lets a terminal, or a shell hook, name the tab itself.
// It outranks every probe so an operator can bind sessions deliberately.
const MeshTerminalIDVariable = "MESH_TERMINAL_ID"

// TerminalIdentity names the terminal tab or pane a Mesh client runs in, so the
// session opened from it can be found again from the same place afterwards.
type TerminalIdentity struct {
	// Key is opaque and stable. It is hashed, so the state directory never
	// carries a terminal's own identifier as a filename.
	Key string
	// Source names the mechanism that supplied it. Diagnostics only.
	Source string
}

// terminalProbes are consulted in order, most durable first. A terminal that
// persists its own pane identifier still names the same pane after the
// application restarts; a tty device only names the same pane until it closes.
//
// Every variable listed for a probe must be present for it to fire. Several
// terminals number their windows from one per instance, so the counter alone
// collides across two windows of the same application; the companion variable
// is what separates them.
var terminalProbes = []struct {
	source    string
	variables []string
}{
	{"mesh", []string{MeshTerminalIDVariable}},
	{"cmux-pane", []string{"CMUX_SURFACE_ID"}},
	{"cmux-tab", []string{"CMUX_TAB_ID"}},
	{"tmux", []string{"TMUX_PANE", "TMUX"}},
	{"zellij", []string{"ZELLIJ_PANE_ID", "ZELLIJ_SESSION_NAME"}},
	{"screen", []string{"WINDOW", "STY"}},
	{"iterm", []string{"ITERM_SESSION_ID"}},
	{"apple-terminal", []string{"TERM_SESSION_ID"}},
	{"wezterm", []string{"WEZTERM_PANE", "WEZTERM_UNIX_SOCKET"}},
	{"kitty", []string{"KITTY_WINDOW_ID", "KITTY_PID"}},
	{"alacritty", []string{"ALACRITTY_WINDOW_ID", "ALACRITTY_SOCKET"}},
	// Ghostty ranks last among terminals: GHOSTTY_SURFACE_ID is a hex address,
	// so a closed surface's value can come back on an unrelated one.
	{"ghostty", []string{"GHOSTTY_SURFACE_ID"}},
}

// InsideMeshSession reports whether this client is running inside a session
// rather than in a terminal of its own. Three signals, because none is
// sufficient alone: a session started by an older Mesh may not export the
// variables, and only the process walk proves it.
func InsideMeshSession() bool {
	if os.Getenv(worker.MeshSessionIDVariable) != "" || os.Getenv(worker.MeshDepthVariable) != "" {
		return true
	}
	return insideSessionProcess()
}

// insideSessionProcess walks the real process tree; tests replace it so their
// verdict does not depend on whether the test runner sits inside a session.
var insideSessionProcess = func() bool {
	_, inside := worker.ContainingSessionWorker()
	return inside
}

// DiscoverTerminal identifies the calling terminal. It reports false when
// nothing names the tab well enough to bind a session to it, and callers then
// keep their previous behaviour rather than guessing: a wrong binding sends a
// tab to somebody else's session, which is worse than no binding at all.
func DiscoverTerminal() (TerminalIdentity, bool) {
	// A client inside a session must never claim the outer tab, or attaching
	// from within a session would rebind the tab the user is sitting in.
	if InsideMeshSession() {
		return TerminalIdentity{}, false
	}
	for _, probe := range terminalProbes {
		values := make([]string, 0, len(probe.variables))
		for _, name := range probe.variables {
			value := strings.TrimSpace(os.Getenv(name))
			if value == "" {
				break
			}
			values = append(values, name+"="+value)
		}
		if len(values) != len(probe.variables) {
			continue
		}
		return TerminalIdentity{Key: terminalKey(probe.source, values), Source: probe.source}, true
	}
	return terminalDeviceIdentity()
}

// terminalDeviceIdentity is the fallback for terminals that publish no pane
// identifier. A tty device number is reused once the tab closes and a pid once
// the shell exits, so the pair is scoped to the current boot: without that, a
// stale pair from a previous boot would match a fresh, unrelated tab. Platforms
// with no boot identifier get no fallback rather than an unsound one.
func terminalDeviceIdentity() (TerminalIdentity, bool) {
	boot := worker.BootID()
	if boot == "" {
		return TerminalIdentity{}, false
	}
	// A character device is not enough: /dev/null is one, and every client
	// redirected from it would share a key and steal each other's bindings.
	if !term.IsTerminal(os.Stdin.Fd()) {
		return TerminalIdentity{}, false
	}
	var status unix.Stat_t
	if err := unix.Fstat(int(os.Stdin.Fd()), &status); err != nil {
		return TerminalIdentity{}, false
	}
	values := []string{
		"boot=" + boot,
		fmt.Sprintf("rdev=%d", status.Rdev),
		fmt.Sprintf("ppid=%d", os.Getppid()),
	}
	return TerminalIdentity{Key: terminalKey("tty", values), Source: "tty"}, true
}

func terminalKey(source string, values []string) string {
	digest := sha256.Sum256([]byte("mesh-terminal-v1\x00" + source + "\x00" + strings.Join(values, "\x00")))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
