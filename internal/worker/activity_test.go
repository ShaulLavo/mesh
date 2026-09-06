package worker

import (
	"sync"
	"testing"
	"time"
)

func TestAttachmentActivitySurvivesTakeoverDetachAndExit(t *testing.T) {
	dir := t.TempDir()
	created := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	meta := Meta{ID: "USED", State: StateDetached, CreatedAt: created}
	if err := WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	w := &Worker{cfg: Config{Dir: dir}, client: &attachment{}, lastAttachedAt: created.Add(time.Hour)}
	w.recordAttachment()
	first := w.lastAttachedAt
	w.lastAttachedAt = first.Add(time.Minute)
	w.recordAttachment()
	w.client = nil
	w.recordAttachment()
	saved, err := ReadMeta(dir)
	if err != nil || saved.State != StateDetached || saved.LastAttachedAt == nil || !saved.LastAttachedAt.Equal(w.lastAttachedAt) {
		t.Fatalf("takeover/detach metadata = %+v, %v", saved, err)
	}
	meta.State = StateExited
	var updates sync.WaitGroup
	for range 8 {
		updates.Go(w.recordAttachment)
	}
	if err := w.recordExit(meta); err != nil {
		t.Fatal(err)
	}
	updates.Wait()
	saved, err = ReadMeta(dir)
	if err != nil || saved.State != StateExited || saved.LastAttachedAt == nil || !saved.LastAttachedAt.Equal(w.lastAttachedAt) {
		t.Fatalf("exit lost activity or was overwritten by detach: %+v, %v", saved, err)
	}
}
