package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/spf13/cobra"
)

func argumentIdentities(t *testing.T) []string {
	t.Helper()
	seeds := [][]byte{bytes.Repeat([]byte{0}, ed25519.SeedSize), bytes.Repeat([]byte{41}, ed25519.SeedSize), make([]byte, ed25519.SeedSize)}
	binary.LittleEndian.PutUint32(seeds[2], 164)
	ids := make([]string, len(seeds))
	for i, seed := range seeds {
		private := ed25519.NewKeyFromSeed(seed)
		ids[i] = base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey))
		public, err := identity.IdentityKey(ids[i])
		if err != nil || !bytes.Equal(public, private.Public().(ed25519.PublicKey)) {
			t.Fatal("fixture identity does not retain its public key")
		}
	}
	if strings.HasPrefix(ids[0], "-") || !strings.HasPrefix(ids[1], "-") || !strings.HasPrefix(ids[2], "--") {
		t.Fatal("fixture identities do not cover the option boundaries")
	}
	return ids
}

func TestCryptographicHostArguments(t *testing.T) {
	for _, id := range argumentIdentities(t) {
		for _, args := range [][]string{{id, "-r"}, {"-r", id}, {id, "--", "echo", id, "-r"}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) { checkHostIdentityArguments(t, id, args) })
		}
	}
}

func checkHostIdentityArguments(t *testing.T, id string, args []string) {
	t.Helper()
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
}

func TestCryptographicDeviceArguments(t *testing.T) {
	for _, id := range argumentIdentities(t) {
		t.Run(id, func(t *testing.T) { checkDeviceIdentityArguments(t, id) })
	}
}

func checkDeviceIdentityArguments(t *testing.T, id string) {
	t.Helper()
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	for _, args := range [][]string{{"device", "approve", id, "--allow-root"}, {"device", "revoke", id}} {
		if _, _, err := executeCommand(t, Dependencies{}, args...); err != nil {
			t.Fatal(err)
		}
		if identity.GrantedIdentity(os.Getenv("MESH_STATE_DIR"), id) != (args[1] == "approve") {
			t.Fatal("grant did not retain the exact device identity")
		}
	}
}

func TestIdentityArgumentsPreserveOptionParsing(t *testing.T) {
	id := argumentIdentities(t)[1]
	tests := []struct {
		name      string
		args      []string
		want      []string
		value     string
		resume    bool
		wantError string
	}{
		{name: "long value", args: []string{"--value", id, id}, want: []string{id}, value: id},
		{name: "short value", args: []string{"-v", id, id}, want: []string{id}, value: id},
		{name: "cluster value", args: []string{"-rv", id, id}, want: []string{id}, value: id, resume: true},
		{name: "attached value", args: []string{"-v" + id, id}, want: []string{id}, value: id},
		{name: "equals value", args: []string{"--value=" + id, id}, want: []string{id}, value: id},
		{name: "delimiter value", args: []string{"--value", "--", id, "-r"}, want: []string{id}, value: "--", resume: true},
		{name: "delimiter payload", args: []string{id, "--", id, "-r"}, want: []string{id, id, "-r"}},
		{name: "unknown long", args: []string{id, "--typo"}, wantError: "unknown flag: --typo"},
		{name: "unknown short", args: []string{id, "-z"}, wantError: "unknown shorthand flag: 'z'"},
		{name: "malformed id", args: []string{"-not-a-key"}, wantError: "unknown shorthand flag: 'n'"},
		{name: "noncanonical id", args: []string{id[:42] + "V"}, wantError: "unknown shorthand flag: 'k'"},
		{name: "help", args: []string{id, "--help"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value string
			var resume bool
			root := &cobra.Command{Use: "mesh", Args: cobra.ArbitraryArgs, SilenceErrors: true, SilenceUsage: true}
			root.Flags().StringVarP(&value, "value", "v", "", "fixture value")
			root.Flags().BoolVarP(&resume, "resume", "r", false, "fixture resume")
			var got []string
			root.RunE = func(_ *cobra.Command, args []string) error { got = slices.Clone(args); return nil }
			command := identityArgumentCommand(root)
			command.SetArgs(test.args)
			err := command.ExecuteContext(t.Context())
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || !slices.Equal(got, test.want) || value != test.value || resume != test.resume {
				t.Fatalf("args=%q value=%q resume=%v error=%v", got, value, resume, err)
			}
		})
	}
}

func TestIdentityArgumentsLeaveProviderPayloadUntouched(t *testing.T) {
	id := argumentIdentities(t)[1]
	root := &cobra.Command{Use: "mesh"}
	var got []string
	root.AddCommand(&cobra.Command{Use: "agent", DisableFlagParsing: true, Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error { got = args; return nil }})
	command := identityArgumentCommand(root)
	command.SetArgs([]string{"agent", "fixture", "--", id, "-r"})
	if err := command.ExecuteContext(t.Context()); err != nil || !slices.Equal(got, []string{"fixture", "--", id, "-r"}) {
		t.Fatalf("provider args=%q error=%v", got, err)
	}
}
