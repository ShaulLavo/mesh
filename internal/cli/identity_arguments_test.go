package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func argumentIdentity(seed byte) string {
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey))
}

func TestCryptographicHostArguments(t *testing.T) {
	for _, seed := range []byte{0, 41} {
		id := argumentIdentity(seed)
		t.Run(id, func(t *testing.T) {
			for _, args := range [][]string{{id, "-r"}, {"-r", id}, {id, "--", "echo", id, "-r"}} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					host := setupCommandTestHost(t)
					host.host.ID, host.host.MeshIdentity = id, id
					if err := saveNamedTestHost(t, host.host); err != nil {
						t.Fatal(err)
					}
					_, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial}, args...)
					if err != nil {
						t.Fatal(err)
					}
					if slices.Contains(args, "--") && !slices.Equal(host.create.Command, []string{"echo", id, "-r"}) {
						t.Fatalf("command payload changed: %q", host.create.Command)
					}
					if !slices.Contains(args, "--") && host.eventCount(protocol.TypeCreate) != 0 {
						t.Fatal("resume flag was not parsed")
					}
				})
			}
		})
	}
}

func TestCryptographicDeviceArguments(t *testing.T) {
	for _, seed := range []byte{0, 41} {
		id := argumentIdentity(seed)
		t.Run(id, func(t *testing.T) {
			t.Setenv("MESH_STATE_DIR", t.TempDir())
			for _, args := range [][]string{{"device", "approve", id, "--allow-root"}, {"device", "revoke", id}} {
				if _, _, err := executeCommand(t, Dependencies{}, args...); err != nil {
					t.Fatal(err)
				}
				if identity.GrantedIdentity(os.Getenv("MESH_STATE_DIR"), id) != (args[1] == "approve") {
					t.Fatal("grant did not retain the exact device identity")
				}
			}
		})
	}
}
