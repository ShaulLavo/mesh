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
		return DefaultDetachKey, false, nil
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
	case c >= '@' && c <= '_': // @ [ \ ] ^ _ and the upper-case letters
		return c - '@', false, nil
	case c == '?':
		return 0x7f, false, nil
	}
	return 0, false, fmt.Errorf("detach key %q: %q is not a control character", s, c)
}
