package execution

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if lockWaitLimit != 1500*time.Millisecond {
		panic("production lock wait limit changed")
	}
	// Instrumented readers must check consistency without a production latency cutoff.
	lockWaitLimit = 2 * time.Minute
	os.Exit(m.Run())
}
