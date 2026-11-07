package execution

import (
	"time"

	"golang.org/x/sys/unix"
)

// ClockAnchor binds elapsed evidence to one boot. Its seal detects damaged anchors.
type ClockAnchor struct {
	Boot    string `json:"boot"`
	Elapsed int64  `json:"elapsed"`
	Seal    string `json:"seal"`
}

func clockEvidence(boot string, elapsed int64) ClockAnchor {
	anchor := ClockAnchor{Boot: boot, Elapsed: elapsed}
	anchor.Seal = digest(struct {
		Boot    string
		Elapsed int64
	}{boot, elapsed})
	return anchor
}
func (a ClockAnchor) valid() bool {
	return a.Boot != "" && a.Elapsed >= 0 && a.Seal == clockEvidence(a.Boot, a.Elapsed).Seal
}
func clockAnchor(boot string) ClockAnchor {
	var t unix.Timespec
	if boot == "" || unix.ClockGettime(unix.CLOCK_MONOTONIC, &t) != nil {
		return ClockAnchor{}
	}
	return clockEvidence(boot, t.Nano())
}
func elapsedFloor(anchor, now ClockAnchor) bool {
	return anchor.valid() && now.valid() && anchor.Boot == now.Boot && now.Elapsed >= anchor.Elapsed && now.Elapsed-anchor.Elapsed >= int64(30*24*time.Hour)
}
