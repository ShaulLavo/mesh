package execution

import (
	"errors"
	"testing"
	"time"
)

func runResolve(c Context) (Intent, map[string]string, error) {
	return Intent{Kind: KindPipe, Command: []string{"/bin/true"}, Cwd: c.Cwd}, nil, nil
}

// A run records its context's use, but not for a caller revoked between the
// lock and the write.
func TestResolveRunRechecksTheGrantBeforeRecordingUse(t *testing.T) {
	j := contextJournal(t)
	c := openTestContext(t, j, ownerA, "ctx", "/work")
	later := c.LastUsedAt.Add(2 * contextUseGranularity)
	j.now = func() time.Time { return later }
	// Another boot's clock makes the recorded use unrecent, so the run writes.
	j.clock = func() ClockAnchor { return clockEvidence("boot-2", int64(time.Hour)) }

	checks := 0
	revokedAfterLock := WithAdmission(t.Context(), func() error {
		checks++
		if checks > 1 {
			return errors.New("grant revoked")
		}
		return nil
	})
	if _, err := j.ResolveRun(revokedAfterLock, ownerA, c.ID, runResolve); err == nil {
		t.Fatal("a run resolved after its grant was revoked")
	}
	if record, err := j.readContext(c.ID); err != nil || !record.LastUsedAt.Equal(c.LastUsedAt) {
		t.Fatalf("revoked run recorded a use: %v, %v", record.LastUsedAt, err)
	}

	snapshot, err := j.ResolveRun(t.Context(), ownerA, c.ID, runResolve)
	if err != nil || snapshot.Intent.Cwd != "/work" || snapshot.Context.ID != c.ID {
		t.Fatalf("resolve = %+v, %v", snapshot, err)
	}
	if record, err := j.readContext(c.ID); err != nil || !record.LastUsedAt.Equal(later.UTC()) {
		t.Fatalf("run did not record its use: %v, %v", record.LastUsedAt, err)
	}
	if _, err := j.ResolveRun(t.Context(), ownerB, c.ID, runResolve); !errors.Is(err, ErrContextNotFound) {
		t.Fatalf("another owner's context = %v", err)
	}
}
