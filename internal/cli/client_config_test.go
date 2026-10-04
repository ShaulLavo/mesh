package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClientCommandRetiresObsoleteAliasState(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	directory := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", directory)
	path := filepath.Join(directory, "hosts.json")
	retained := []byte(`{"version":1,"hosts":[{"alias":"obsolete","id":"owner","meshIdentity":"pin","endpoint":"ws://127.0.0.1:7777/mesh","addresses":["127.0.0.1"]}],"dashboard":{"theme":"oled"}}`)
	if err := os.WriteFile(path, retained, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "device", "identity", "--json"); err != nil {
		t.Fatal(err)
	}
	after, err := readClientConfigTestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var beforeObject, afterObject map[string]any
	if err := json.Unmarshal(retained, &beforeObject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &afterObject); err != nil {
		t.Fatal(err)
	}
	delete(beforeObject["hosts"].([]any)[0].(map[string]any), "alias")
	expected, err := json.Marshal(beforeObject)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(afterObject)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("client activation did not delete only obsolete aliases")
	}
	if _, err := LoadHosts(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCommandRefusesUnknownConfigWithoutMutation(t *testing.T) {
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	directory := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", directory)
	path := filepath.Join(directory, "hosts.json")
	retained := []byte(`{"version":1,"hosts":[],"unexpected":true}`)
	if err := os.WriteFile(path, retained, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "device", "identity", "--json"); err == nil {
		t.Fatal("client accepted unrecognized configuration")
	}
	after, err := readClientConfigTestFile(path)
	if err != nil || !bytes.Equal(after, retained) {
		t.Fatal("failed retirement changed unrecognized configuration")
	}
}
