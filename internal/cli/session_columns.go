package cli

import (
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

// formatBytes renders memory the way ps and top users read it: binary units,
// one decimal below ten so 1.2G and 412M both stay three or four columns.
func formatBytes(bytes uint64) string {
	if bytes == 0 {
		return "-"
	}
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	value := float64(bytes)
	for _, unit := range []string{"K", "M", "G", "T", "P"} {
		value /= 1024
		// Rounding can carry 1023.96K up to "1024K"; the next unit reads better.
		if value < 9.95 {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
		if value < 1023.5 || unit == "P" {
			return fmt.Sprintf("%.0f%s", value, unit)
		}
	}
	return "-"
}

// sessionMemory is the MEM cell. Only a live session holds memory, and an
// older host that cannot measure reports zero.
func sessionMemory(row protocol.SessionInfo) string {
	if !liveState(row.State) {
		return "-"
	}
	return formatBytes(row.MemoryBytes)
}

// lastOutputAt is when a session last printed, or when it started if the host
// has not recorded any output.
func lastOutputAt(row protocol.SessionInfo) time.Time {
	if row.Recovery != nil && row.Recovery.LastOutputAt.After(row.CreatedAt) {
		return row.Recovery.LastOutputAt
	}
	return row.CreatedAt
}

// sessionIdle is the IDLE cell: quiet time for a live session, sleep time for
// a hibernated one, and nothing for a session that is simply over.
func sessionIdle(now time.Time, row protocol.SessionInfo) string {
	if marker := Hibernation(row); marker != nil {
		return ageAt(now, marker.At)
	}
	if !liveState(row.State) {
		return "-"
	}
	return ageAt(now, lastOutputAt(row))
}
