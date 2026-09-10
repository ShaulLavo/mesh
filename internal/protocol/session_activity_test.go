package protocol

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/recovery"
)

func TestSessionLastActiveAtIncludesSavedUpdates(t *testing.T) {
	created := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	attached := created.Add(time.Hour)
	checkpoint := created.Add(24 * time.Hour)
	for _, test := range []struct {
		name string
		row  SessionInfo
		want time.Time
	}{
		{name: "legacy creation", row: SessionInfo{CreatedAt: created}, want: created},
		{name: "attachment", row: SessionInfo{CreatedAt: created, LastAttachedAt: &attached}, want: attached},
		{name: "saved update", row: SessionInfo{CreatedAt: created, LastAttachedAt: &attached, Recovery: &recovery.Record{CheckpointAt: checkpoint}}, want: checkpoint},
		{name: "output", row: SessionInfo{CreatedAt: created, Recovery: &recovery.Record{LastOutputAt: checkpoint}}, want: checkpoint},
		{name: "launch fallback", row: SessionInfo{CreatedAt: created, Recovery: &recovery.Record{}}, want: created},
		{name: "attachment after checkpoint", row: SessionInfo{CreatedAt: created, LastAttachedAt: &checkpoint, Recovery: &recovery.Record{CheckpointAt: attached}}, want: checkpoint},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.row.LastActiveAt(); !got.Equal(test.want) {
				t.Fatalf("last activity = %s, want %s", got, test.want)
			}
		})
	}
}
