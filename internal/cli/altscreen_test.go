package cli

import "testing"

func TestAltScreenTrackerFollowsTheSession(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		chunks []string
		want   bool
	}{
		{"a plain shell never enters", []string{"$ ls\r\n", "file\r\n"}, false},
		{"entering leaves it active", []string{"\x1b[?1049hdrawing"}, true},
		{"the program exiting clears it", []string{"\x1b[?1049hdraw", "more\x1b[?1049l$ "}, false},
		{"re-entering after leaving", []string{"\x1b[?1049l", "\x1b[?1049h"}, true},
		{"both in one chunk, last wins", []string{"\x1b[?1049l\x1b[?1049h"}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			var tracker altScreenTracker
			for _, chunk := range row.chunks {
				tracker.Observe([]byte(chunk))
			}
			if tracker.Active() != row.want {
				t.Fatalf("Active() = %v, want %v", tracker.Active(), row.want)
			}
		})
	}
}

func TestAltScreenTrackerSeesASequenceSplitAcrossFrames(t *testing.T) {
	t.Parallel()

	// The host writes whenever its buffer fills, so an escape sequence can be
	// torn across two frames. Missing it would strand the terminal in the
	// alternate buffer on detach, which is the whole bug.
	for split := 1; split < len(enterAltScreenSequence); split++ {
		var tracker altScreenTracker
		tracker.Observe([]byte("before" + enterAltScreenSequence[:split]))
		tracker.Observe([]byte(enterAltScreenSequence[split:] + "after"))
		if !tracker.Active() {
			t.Fatalf("split after %d bytes was missed", split)
		}
	}
}
