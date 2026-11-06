package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
)

func TestProfileDecodedGoldens(t *testing.T) {
	state := t.TempDir()
	config := t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := runProfile(t, bytes.NewBufferString(`{"schemaVersion":1,"description":"Host notes","instructions":"one\ntwo"}`), "profile", "set", "--file", "-", "--expected-revision", "0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := hostprofile.OpenHostStore(config, host.ID).Read()
	if err != nil {
		t.Fatal(err)
	}
	var write agentapi.ProfileWrite
	decodeProfileData(t, result.Data, &write)
	if write.Digest != snapshot.Value.Digest() || !write.Committed || !write.Changed || write.Revision != 1 {
		t.Fatal(write)
	}
	write.Digest = "DIGEST"
	result.Data = write
	assertProfileGolden(t, "profile.set", result)
	result, _, err = runProfile(t, noProfileInput{t}, "profile", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var section agentapi.ProfileSection
	decodeProfileData(t, result.Data, &section)
	if section.Digest != snapshot.Value.Digest() || section.UpdatedAt == nil || !section.UpdatedAt.Equal(snapshot.Value.UpdatedAt) || section.Profile.Instructions == nil {
		t.Fatal(section)
	}
	section.Digest = "DIGEST"
	section.UpdatedAt = nil
	result.Data = section
	assertProfileGolden(t, "profile.show", result)
	result, _, err = runProfile(t, noProfileInput{t}, "profile", "edit", "--json")
	if code, ok := StatusCode(err); !ok || code != 2 {
		t.Fatal(err)
	}
	assertProfileGolden(t, "profile.edit.usage", result)
}

func decodeProfileData(t *testing.T, data any, target any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(target); err != io.EOF {
		t.Fatalf("extra profile data: %v", err)
	}
}

func assertProfileGolden(t *testing.T, name string, envelope agentapi.Envelope) {
	t.Helper()
	envelope.RequestID = "01JA0000000000000000000000"
	var output bytes.Buffer
	if err := agentapi.Write(&output, envelope); err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, output.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "agentapi", "testdata", name+".json")
	if os.Getenv("MESH_UPDATE_AGENT_GOLDENS") == "1" {
		if err := os.WriteFile(path, pretty.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// #nosec G304 -- Fixed testdata path from the table in this test.
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("%s changed:\n%s", name, pretty.String())
	}
}
