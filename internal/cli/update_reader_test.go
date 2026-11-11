package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/update"
)

func TestUpdateReaderRegistrationIsOfflineAndHidden(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_CONFIG_DIR", blocked)
	t.Setenv("MESH_STATE_DIR", blocked)
	command := NewCommand(Dependencies{})
	reader, _, err := command.Find([]string{"update-reader"})
	if err != nil || reader.Name() != "update-reader" || !reader.Hidden {
		t.Fatalf("reader registration: %v %v", reader, err)
	}
	stdout, _, err := executeCommand(t, Dependencies{}, "update-reader", "-capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var capabilities update.HostReaderCapabilities
	if err := json.Unmarshal([]byte(stdout), &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Schema != update.HostReaderSchema || capabilities.Epoch != update.HostReaderEpoch || capabilities.Build.Digest == "" {
		t.Fatalf("capabilities: %+v", capabilities)
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	data, err := directory.ReadFile("not-a-directory")
	if err != nil || string(data) != "preserve" {
		t.Fatalf("reader changed user configuration: %q %v", data, err)
	}
}

func TestUpdateReaderRejectsIncompleteModes(t *testing.T) {
	for _, arguments := range [][]string{nil, {"-capabilities", "-input", "packet"}, {"-capabilities", "operand"}, {"-input", "packet"}, {"-input", "packet", "-trust-cache", "relative"}, {"-root", "arbitrary"}} {
		_, _, err := executeCommand(t, Dependencies{}, append([]string{"update-reader"}, arguments...)...)
		if err == nil {
			t.Fatalf("accepted incomplete or unsupported reader mode: %v", arguments)
		}
	}
}
