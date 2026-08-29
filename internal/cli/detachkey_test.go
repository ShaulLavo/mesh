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
