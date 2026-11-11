package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHelperJournalProbeUsesActualDecoder(t *testing.T) {
	for _, schema := range []int{1, 2} {
		t.Run(strconv.Itoa(schema), func(t *testing.T) { checkHelperJournalSchema(t, schema) })
	}
}

func checkHelperJournalSchema(t *testing.T, schema int) {
	t.Helper()
	state := t.TempDir()
	status := updateinstall.Status{Schema: schema, Phase: updateinstall.Committed,
		Settings: updateinstall.Settings{StateDir: state, Executable: filepath.Join(state, "mesh"),
			CacheDir: filepath.Join(state, "cache"), ClientOnly: true}}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "update")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	err = checkUpdateJournal(state)
	if (err == nil) != (schema == 1) {
		t.Fatalf("journal schema %d probe = %v", schema, err)
	}
}
