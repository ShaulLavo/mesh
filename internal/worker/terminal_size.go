package worker

// ReadSessionLeaderTerminalSize reads the current terminal dimensions from a
// live session leader's standard input. It never writes to the terminal,
// changes its size, or acquires it as this process's controlling terminal.
// False means the process or its bounded terminal dimensions were unavailable.
func ReadSessionLeaderTerminalSize(pid int) (cols, rows int, ok bool) {
	if pid <= 0 {
		return 0, 0, false
	}
	cols, rows, ok = readSessionLeaderTerminalSize(pid)
	if !ok {
		return 0, 0, false
	}
	return observedTerminalSize(cols, rows)
}

func observedTerminalSize(cols, rows int) (int, int, bool) {
	if validateTerminalSize(cols, rows) != nil {
		return 0, 0, false
	}
	return cols, rows, true
}
