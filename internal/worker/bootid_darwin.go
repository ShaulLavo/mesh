//go:build darwin

package worker

import (
	"strings"

	"golang.org/x/sys/unix"
)

func platformBootID() string {
	// The kernel boot-session UUID avoids treating a wall-clock adjustment as
	// proof that an uncertain replacement worker can no longer be alive.
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}
