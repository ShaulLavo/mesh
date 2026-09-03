package cli

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestRenameHost(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", dir)

	identity := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	other := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	for alias, id := range map[string]string{"omarchy": identity, "pi": other} {
		if err := SaveHost(HostRecord{
			Alias: alias, ID: id, MeshIdentity: id,
			Endpoint: "ws://127.0.0.1:7337/mesh",
		}); err != nil {
			t.Fatal(err)
		}
	}

	renamed, err := RenameHost("omarchy", "pc")
	if err != nil {
		t.Fatalf("RenameHost() = %v", err)
	}
	if renamed.Alias != "pc" {
		t.Fatalf("alias = %q", renamed.Alias)
	}

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	// The old name must be gone, not left beside the new one: SaveHost matches
	// on identity and would otherwise keep both.
	var names []string
	for _, host := range hosts {
		names = append(names, host.Alias)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %v, want two", names)
	}
	for _, host := range hosts {
		if host.Alias == "omarchy" {
			t.Fatalf("the old name survived: %v", names)
		}
	}

	// A name in use by another host is refused rather than colliding.
	if _, err := RenameHost("pc", "pi"); err == nil {
		t.Fatal("renaming onto another host's name was allowed")
	}
	// Renaming to the same name is not an error.
	if _, err := RenameHost("pc", "pc"); err != nil {
		t.Fatalf("renaming to the same name = %v", err)
	}
	if _, err := RenameHost("nonexistent", "x"); err == nil {
		t.Fatal("renaming an unknown host was allowed")
	}
}
