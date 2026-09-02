package cli

import "bytes"

const (
	enterAltScreenSequence = "\x1b[?1049h"
	leaveAltScreenSequence = "\x1b[?1049l"
)

// altScreenTracker follows whether the session has put this terminal into the
// alternate buffer. Detaching leaves the remote program running, so it never
// emits the sequence that would bring the terminal back, and only a session
// that actually entered the buffer should be pulled out of it: restoring
// unconditionally would move the cursor of a plain shell session, because
// leaving the alternate buffer also restores a saved cursor position.
type altScreenTracker struct {
	active bool
	carry  []byte // trailing bytes that may begin a sequence split across frames
}

// Observe folds one chunk of session output into the tracker.
func (t *altScreenTracker) Observe(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	scan := chunk
	if len(t.carry) > 0 {
		scan = append(append([]byte(nil), t.carry...), chunk...)
	}
	enter := bytes.LastIndex(scan, []byte(enterAltScreenSequence))
	leave := bytes.LastIndex(scan, []byte(leaveAltScreenSequence))
	if enter >= 0 || leave >= 0 {
		t.active = enter > leave
	}

	// Keep enough of the tail to complete a sequence that straddles two frames.
	keep := len(enterAltScreenSequence) - 1
	if len(scan) < keep {
		keep = len(scan)
	}
	t.carry = append(t.carry[:0], scan[len(scan)-keep:]...)
}

// Active reports whether the terminal is currently in the alternate buffer.
func (t *altScreenTracker) Active() bool { return t.active }
