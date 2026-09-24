package update

import (
	"testing"
	"time"
)

func TestAnAuthorizedTargetThatDropsOffIsPolledAgainQuickly(t *testing.T) {
	if got := offlineRetryDelay(Target{Grant: true}); got != restartRetry {
		t.Fatalf("authorized target retry = %s, want %s: its restart is the expected cause", got, restartRetry)
	}
	if got := offlineRetryDelay(Target{}); got != offlineRetry {
		t.Fatalf("unauthorized target retry = %s, want %s", got, offlineRetry)
	}
	if restartRetry >= offlineRetry || restartRetry > 5*time.Second {
		t.Fatalf("restart retry %s should be short", restartRetry)
	}
}
