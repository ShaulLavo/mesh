//go:build linux

package worker

import "golang.org/x/sys/unix"

// setHugePages opts this process in or out of transparent huge pages. Kernels
// built without THP reject the call, which is fine.
func setHugePages(enabled bool) {
	disable := uintptr(1)
	if enabled {
		disable = 0
	}
	_ = unix.Prctl(unix.PR_SET_THP_DISABLE, disable, 0, 0, 0)
}

func hugePagesDisabled() bool {
	disabled, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
	return err == nil && disabled != 0
}
