package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

func TestScopedOutputExcludesPipeAndLineAllocations(t *testing.T) {
	for _, purpose := range []session.Purpose{session.PipeExecution, session.LineExecution} {
		t.Run(string(purpose), func(t *testing.T) {
			f := newScopedFixture(t.Context(), t, false)
			enableScopedOutput(t, f)
			sessions := compactSocketTempDir(t)
			f.server.lifecycle.sessionsDir = sessions
			ref, err := worker.Reserve(sessions, purpose)
			if err != nil {
				t.Fatal(err)
			}
			f.catalog.rows[0].ID = storage.SessionID(ref.ID)
			f.catalog.rows[0].State = storage.StateExited
			if err := os.WriteFile(paths.Log(filepath.Join(sessions, ref.ID)), []byte(disclosureSentinel), 0600); err != nil {
				t.Fatal(err)
			}
			for _, request := range []protocol.AccessOutput{
				{SessionID: ref.ID, Format: "tail"},
				{SessionID: ref.ID, Format: "preview", Cols: 1, Rows: 1},
			} {
				result := scopedOutput(t, f, request)
				if result.ErrorCode != "session.not_found" || result.Result != nil {
					t.Fatalf("private worker admitted to scoped output: %+v", result)
				}
			}
		})
	}
}
