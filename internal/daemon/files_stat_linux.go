//go:build linux

package daemon

import "syscall"

func statChangeNanos(stat *syscall.Stat_t) int64 { return stat.Ctim.Nano() }
