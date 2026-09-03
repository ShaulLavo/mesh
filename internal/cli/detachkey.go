package cli

import (
	"fmt"
	"strings"
)

// ParseDetachKey accepts "ctrl+]" style names, a literal "^]", or "none" to
// disable interception. It exists so the one keystroke Mesh steals from your
// programs is yours to choose.
func ParseDetachKey(s string) (key byte, raw bool, err error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "":
		// Not DefaultDetachKey: a client running inside a session must listen
		// for a key the clients above it will forward rather than one they
		// intercept.
		return DetachKeyForDepth(SessionDepth()), false, nil
	case "none", "off":
		return 0, true, nil
	}

	name := strings.TrimPrefix(s, "^")
	if name == s {
		for _, p := range []string{"ctrl+", "ctrl-", "c-"} {
			if rest, found := strings.CutPrefix(s, p); found {
				name = rest
				break
			}
		}
	}
	if len(name) != 1 {
		return 0, false, fmt.Errorf("detach key %q: want something like ctrl+] or none", s)
	}

	c := name[0]
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 1, false, nil
	case c == '@':
		// ctrl+@ is NUL, and byte 0 is how the attach path spells "no detach key
		// configured". Accepting it would silently reinstate the default ctrl+]
		// that the user was trying to free, with no diagnostic.
		return 0, false, fmt.Errorf("detach key %q: ctrl+@ is NUL, which cannot be distinguished from no key; use --raw to disable the detach key", s)
	case c > '@' && c <= '_': // [ \ ] ^ _ and the upper-case letters
		return c - '@', false, nil
	case c == '?':
		return 0x7f, false, nil
	}
	return 0, false, fmt.Errorf("detach key %q: %q is not a control character", s, c)
}
