//go:build linux

package worker

import "os"

func platformBootID() string {
	if contents, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return string(trimSpace(contents))
	}
	return ""
}
