package cli

import "testing"

func TestParseDetachKey(t *testing.T) {
	cases := []struct {
		in   string
		key  byte
		raw  bool
		fail bool
	}{
		{in: "", key: DefaultDetachKey},
		{in: "ctrl+]", key: 0x1d},
		{in: "Ctrl-]", key: 0x1d},
		{in: "c-]", key: 0x1d},
		{in: "^]", key: 0x1d},
		{in: `ctrl+\`, key: 0x1c},
		{in: "ctrl+a", key: 0x01},
		{in: "none", raw: true},
		{in: "ctrl+1", fail: true},
		{in: "ctrl+abc", fail: true},
	}
	for _, c := range cases {
		key, raw, err := ParseDetachKey(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("ParseDetachKey(%q) = %#x, want error", c.in, key)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDetachKey(%q): %v", c.in, err)
			continue
		}
		if key != c.key || raw != c.raw {
			t.Errorf("ParseDetachKey(%q) = %#x, raw=%v; want %#x, raw=%v", c.in, key, raw, c.key, c.raw)
		}
	}
}

// ctrl+@ is NUL, and byte 0 is the attach path's sentinel for "no detach key".
// Accepting it silently reinstated ctrl+], the key the user was freeing.
func TestParseDetachKeyRejectsNul(t *testing.T) {
	for _, spelling := range []string{"ctrl+@", "^@", "C-@"} {
		if _, _, err := ParseDetachKey(spelling); err == nil {
			t.Fatalf("ParseDetachKey(%q) was accepted; NUL is indistinguishable from no key", spelling)
		}
	}
	// The neighbouring control characters must still parse.
	for spelling, want := range map[string]byte{"ctrl+]": 0x1d, "ctrl+a": 0x01, "ctrl+_": 0x1f} {
		got, raw, err := ParseDetachKey(spelling)
		if err != nil {
			t.Fatalf("ParseDetachKey(%q) error = %v", spelling, err)
		}
		if raw {
			t.Fatalf("ParseDetachKey(%q) reported raw mode", spelling)
		}
		if got != want {
			t.Fatalf("ParseDetachKey(%q) = %#x, want %#x", spelling, got, want)
		}
	}
}
