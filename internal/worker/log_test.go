package worker

import (
	"os"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/paths"
)

func TestReadLogTailReadsOnlyTheRequestedSuffix(t *testing.T) {
	dir := t.TempDir()
	contents := strings.Repeat("prefix-", 100) + "wanted suffix"
	if err := os.WriteFile(paths.Log(dir), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadLogTail(dir, len("wanted suffix"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "wanted suffix" {
		t.Fatalf("log tail = %q", got)
	}
}
