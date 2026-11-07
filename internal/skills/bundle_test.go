package skills

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/skills/internal/reference"
)

func TestBundleReferenceAndGenericBytes(t *testing.T) {
	commands, err := Show("references/commands.md")
	must(t, err)
	if string(commands) != reference.Render() {
		t.Fatal("command reference drift; run go generate ./internal/skills")
	}
	files := Bundle()
	total := 0
	for _, file := range files {
		total += len(file.Data)
		if !validPath(file.Path) {
			t.Fatal("invalid embedded path")
		}
		if regexp.MustCompile(`/home/|/Users/|sprockt|shaul|[a-z0-9-]+\.(dev|com|net)\b`).Match(file.Data) {
			t.Fatalf("personal or real-looking hostname in %s", file.Path)
		}
	}
	if total > maximumBundle {
		t.Fatal("bundle exceeds 256 KiB")
	}
	if !strings.Contains(string(files[0].Data), "mesh-api: \"1\"") {
		t.Fatal("missing skill API metadata")
	}
	for _, operation := range agentapi.Operations {
		if !operation.Implemented && bytes.Contains(commands, []byte("## "+operation.Command+"\n")) {
			t.Fatal("future command advertised")
		}
	}
	original := Digest()
	files[0].Data[0] ^= 1
	if Digest() != original {
		t.Fatal("caller mutated embedded bytes")
	}
	if _, err := Show("../SKILL.md"); err == nil {
		t.Fatal("unknown embedded path accepted")
	}
}
