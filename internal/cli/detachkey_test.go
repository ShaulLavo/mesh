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

func TestDetachKeyPerNestingLevel(t *testing.T) {
	// The outermost client reads every keystroke first, so each level must
	// listen for a different key: the levels above forward one that is not
	// theirs, and only the level that owns it detaches.
	keys := map[int]byte{}
	for depth := 0; depth < 3; depth++ {
		keys[depth] = DetachKeyForDepth(depth)
	}
	if keys[0] != DefaultDetachKey {
		t.Fatalf("depth 0 = %#x, want the documented ctrl+]", keys[0])
	}
	if keys[0] == keys[1] || keys[1] == keys[2] {
		t.Fatalf("levels share a key: %#x %#x %#x", keys[0], keys[1], keys[2])
	}
	// Nesting past the table shares the last key rather than listening for
	// nothing.
	if DetachKeyForDepth(9) != keys[2] {
		t.Fatalf("deep nesting = %#x, want the last key %#x", DetachKeyForDepth(9), keys[2])
	}
	if DetachKeyForDepth(-1) != keys[0] {
		t.Fatalf("negative depth = %#x, want the outermost key", DetachKeyForDepth(-1))
	}
}

func TestFlaglessDetachKeyFollowsDepth(t *testing.T) {
	t.Setenv("MESH_DEPTH", "1")
	key, raw, err := ParseDetachKey("")
	if err != nil || raw {
		t.Fatalf("ParseDetachKey() = %#x, raw %v, %v", key, raw, err)
	}
	if key != DetachKeyForDepth(1) {
		t.Fatalf("key = %#x, want the depth-1 key %#x", key, DetachKeyForDepth(1))
	}

	// An explicit choice still wins at any depth.
	chosen, _, err := ParseDetachKey("ctrl+]")
	if err != nil {
		t.Fatal(err)
	}
	if chosen != DefaultDetachKey {
		t.Fatalf("explicit key = %#x", chosen)
	}
}

func TestSessionDepthIgnoresNonsense(t *testing.T) {
	for _, value := range []string{"", "not-a-number", "-3"} {
		t.Setenv("MESH_DEPTH", value)
		if got := SessionDepth(); got != 0 {
			t.Fatalf("SessionDepth() with %q = %d, want 0", value, got)
		}
	}
	t.Setenv("MESH_DEPTH", "2")
	if got := SessionDepth(); got != 2 {
		t.Fatalf("SessionDepth() = %d, want 2", got)
	}
}
