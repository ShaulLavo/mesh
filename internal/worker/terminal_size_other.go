//go:build !linux && !darwin

package worker

func readSessionLeaderTerminalSize(int) (cols, rows int, ok bool) {
	return 0, 0, false
}
