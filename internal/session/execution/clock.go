package execution

import (
	"golang.org/x/sys/unix"
	"time"
)

type ClockAnchor struct {
	Boot    string `json:"boot"`
	Elapsed int64  `json:"elapsed"`
}

func clockAnchor(boot string) ClockAnchor {
	var t unix.Timespec
	if boot == "" || unix.ClockGettime(unix.CLOCK_MONOTONIC, &t) != nil {
		return ClockAnchor{}
	}
	return ClockAnchor{Boot: boot, Elapsed: t.Nano()}
}
func elapsedFloor(anchor ClockAnchor, now ClockAnchor) bool {
	return anchor.Boot != "" && anchor.Boot == now.Boot && anchor.Elapsed >= 0 && now.Elapsed >= anchor.Elapsed && now.Elapsed-anchor.Elapsed >= int64(30*24*time.Hour)
}
