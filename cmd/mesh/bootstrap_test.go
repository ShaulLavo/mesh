package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func isolateBootstrapState(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	stateDir := t.TempDir()
	t.Setenv("MESH_STATE_DIR", stateDir)
	return stateDir
}

func TestBootstrapFuncPinsExistingIdentityAndMapsResult(t *testing.T) {
	stateDir := isolateBootstrapState(t)
	identity := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := cli.SaveHost(cli.HostRecord{
		MachineName: "pc", ID: identity, MeshIdentity: identity,
		Addresses: []string{"100.64.0.1"}, Endpoint: "ws://100.64.0.1:7337/mesh",
	}); err != nil {
		t.Fatal(err)
	}

	path, err := cli.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := machinename.RememberClaim(t.Context(), filepath.Dir(path), identity, machinename.Claim{ID: identity, MachineName: "pc", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	var captured bootstrap.Options
	var progress bytes.Buffer
	bootstrapFunc := newBootstrapFunc(func(_ context.Context, opts bootstrap.Options) (bootstrap.Result, error) {
		captured = opts
		opts.Progress(bootstrap.Event{Step: bootstrap.StepConnect, Detail: "alice@pc"})
		return bootstrap.Result{
			AuthenticatedIdentity: identity, ID: identity, MeshIdentity: identity, TailscaleName: "pc.example.ts.net",
			TailscaleAddresses: []string{"100.64.0.2"}, Endpoint: "ws://100.64.0.2:7337/mesh",
			AlreadyConfigured: true,
		}, nil
	}, bootstrapUI{
		input: io.Reader(strings.NewReader("")), output: &progress,
		username: func() (string, error) { return "alice", nil },
	})

	result, err := bootstrapFunc(context.Background(), cli.AddRequest{Target: "100.64.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Target != "100.64.0.1" || captured.StateDir != stateDir || captured.ExpectedIdentity != identity {
		t.Fatalf("bootstrap options = %#v", captured)
	}
	if captured.SSH.Password == nil || captured.SSH.Passphrase == nil || captured.SSH.ConfirmHostKey == nil || captured.ConfirmProvision == nil || captured.SudoPassword == nil {
		t.Fatalf("SSH prompt callbacks = %#v, want all callbacks", captured.SSH)
	}
	if result.AuthenticatedIdentity != identity || result.Host.ID != identity || result.Host.TailscaleName != "pc.example.ts.net" || result.Host.Endpoint != "ws://100.64.0.2:7337/mesh" || !result.AlreadyConfigured {
		t.Fatalf("bootstrap result = %#v", result)
	}
	if got, want := progress.String(), "BOOTSTRAP connect   alice@pc\n"; got != want {
		t.Fatalf("progress = %q, want %q", got, want)
	}
}

func TestSudoPasswordPromptRefusesNonTerminalInput(t *testing.T) {
	_, err := sudoPasswordPrompt(bootstrapUI{terminal: func() bool { return false }})(context.Background(), "alice@pi")
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("sudoPasswordPrompt() error = %v", err)
	}
}

func TestBootstrapFuncReadsAuthKeyAndYesApprovesWithoutPrompt(t *testing.T) {
	isolateBootstrapState(t)
	keyPath := t.TempDir() + "/tailscale.key"
	const secret = "tskey-auth-bootstrap-fixture" //nolint:gosec // inert sentinel proves the adapter does not record the key
	if err := os.WriteFile(keyPath, []byte("  "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var output bytes.Buffer
	bootstrapFunc := newBootstrapFunc(func(ctx context.Context, opts bootstrap.Options) (bootstrap.Result, error) {
		if string(opts.TailscaleAuthKey) != secret {
			t.Fatalf("auth key = %q", opts.TailscaleAuthKey)
		}
		approved, err := opts.ConfirmProvision(ctx, bootstrap.ProvisionConfirmation{Summary: "install", PackageManager: "pacman", Commands: []string{"pacman"}})
		if err != nil || !approved {
			t.Fatalf("--yes confirmation = %t, %v", approved, err)
		}
		opts.Progress(bootstrap.Event{Step: bootstrap.StepProvision, Detail: "authenticate Tailscale from standard input"})
		return bootstrap.Result{
			AuthenticatedIdentity: identity, ID: identity, MeshIdentity: identity, TailscaleName: "pi.example.ts.net",
			TailscaleAddresses: []string{"100.64.0.8"}, Endpoint: "ws://100.64.0.8:7337/mesh",
		}, nil
	}, bootstrapUI{
		input: strings.NewReader(""), output: &output,
		username: func() (string, error) { return "alice", nil },
		terminal: func() bool { return false },
	})
	result, err := bootstrapFunc(context.Background(), cli.AddRequest{
		Target: "pi", TailscaleAuthKeyFile: keyPath, Yes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorded := output.String() + result.Host.ID + result.Host.TailscaleName + result.Host.Endpoint + strings.Join(result.Host.Addresses, "")
	if strings.Contains(recorded, secret) {
		t.Fatalf("auth key leaked into bootstrap output or result: %q", recorded)
	}
}

func TestProvisionPromptNumbersPlainActions(t *testing.T) {
	var output bytes.Buffer
	confirmed, err := provisionPrompt(bootstrapUI{
		input: strings.NewReader("yes\n"), output: &output,
		terminal: func() bool { return true },
	})(context.Background(), bootstrap.ProvisionConfirmation{
		Summary: "pi (alice@10.0.0.9) is running Debian. Install Tailscale with pacman?", PackageManager: "pacman",
		Actions: []bootstrap.ProvisionAction{
			{Description: "install Tailscale with pacman", Command: "sudo pacman -S --needed --noconfirm tailscale"},
			{Description: "start Tailscale now and at boot", Command: "sudo systemctl enable --now tailscaled"},
		},
		Checks: []string{"sudo password must authenticate before any remote change"},
	})
	if err != nil || !confirmed {
		t.Fatalf("provisionPrompt() = %t, %v", confirmed, err)
	}
	got := output.String()
	for _, want := range []string{
		"pi (alice@10.0.0.9) is running Debian",
		"1. install Tailscale with pacman",
		"sudo pacman -S --needed --noconfirm tailscale",
		"2. start Tailscale now and at boot",
		"first: sudo password must authenticate",
		"Continue? [y/N]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt = %q, want %q", got, want)
		}
	}
	// The password plumbing that delivers a sudo password is not a change an
	// operator approves, and must never reach the prompt.
	if strings.Contains(got, "mesh_sudo_password") {
		t.Fatalf("prompt leaked the password wrapper: %q", got)
	}
}

func TestProvisionPromptRefusesNonTerminalInput(t *testing.T) {
	_, err := provisionPrompt(bootstrapUI{
		input: strings.NewReader("yes\n"), output: io.Discard,
		terminal: func() bool { return false },
	})(context.Background(), bootstrap.ProvisionConfirmation{})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal or --yes") {
		t.Fatalf("provisionPrompt() error = %v", err)
	}
}

func TestReadTailscaleAuthKeyRejectsEmptyAndOversizedFiles(t *testing.T) {
	for name, contents := range map[string][]byte{
		"empty":     []byte(" \n\t"),
		"oversized": bytes.Repeat([]byte("x"), maximumTailscaleAuthKeyFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := t.TempDir() + "/key"
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readTailscaleAuthKey(path); err == nil {
				t.Fatalf("readTailscaleAuthKey(%s) succeeded", name)
			}
		})
	}
}

func TestCommandDependenciesWireBootstrapAndPicker(t *testing.T) {
	dependencies := commandDependencies()
	if dependencies.Bootstrap == nil || dependencies.Picker == nil {
		t.Fatalf("command dependencies = %#v, want bootstrap and picker", dependencies)
	}
}

func TestPromptedAuthKeyIsRedactedLikeAFileKey(t *testing.T) {
	isolateBootstrapState(t)
	const secret = "tskey-pasted-not-from-a-file" //nolint:gosec // inert sentinel verifies output redaction
	identity := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var output bytes.Buffer
	bootstrapFunc := newBootstrapFunc(func(ctx context.Context, opts bootstrap.Options) (bootstrap.Result, error) {
		if len(opts.TailscaleAuthKey) != 0 {
			t.Fatalf("static auth key = %q, want none", opts.TailscaleAuthKey)
		}
		if opts.TailscaleAuthKeyPrompt == nil {
			t.Fatal("no auth key prompt was wired")
		}
		// Without a terminal the prompt must name the flag rather than hang.
		if _, err := opts.TailscaleAuthKeyPrompt(ctx, "pi"); err == nil ||
			!strings.Contains(err.Error(), "--tailscale-auth-key-file") {
			t.Fatalf("non-terminal prompt error = %v", err)
		}
		opts.Progress(bootstrap.Event{Step: bootstrap.StepProvision, Detail: secret})
		return bootstrap.Result{
			AuthenticatedIdentity: identity, ID: identity, MeshIdentity: identity, TailscaleName: "pi.example.ts.net",
			TailscaleAddresses: []string{"100.64.0.8"}, Endpoint: "ws://100.64.0.8:7337/mesh",
		}, nil
	}, bootstrapUI{
		input: strings.NewReader(""), output: &output,
		username: func() (string, error) { return "alice", nil },
		terminal: func() bool { return false },
	})
	if _, err := bootstrapFunc(context.Background(), cli.AddRequest{Target: "pi", Yes: true}); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapPassesTheTargetThroughUntouched(t *testing.T) {
	isolateBootstrapState(t)
	// The user must be decided after ~/.ssh/config is read. Prepending the
	// local username here made `mesh add pi` connect as whoever was typing,
	// beating a `User pi` the config already had right.
	var captured bootstrap.Options
	bootstrapFunc := newBootstrapFunc(func(_ context.Context, opts bootstrap.Options) (bootstrap.Result, error) {
		captured = opts
		return bootstrap.Result{}, errors.New("stop after the target is set")
	}, bootstrapUI{
		input: strings.NewReader(""), output: io.Discard,
		username: func() (string, error) { return "whoever", nil },
		terminal: func() bool { return false },
	})
	_, _ = bootstrapFunc(context.Background(), cli.AddRequest{Target: "pi"})
	if captured.Target != "pi" {
		t.Fatalf("target = %q, want it passed through untouched", captured.Target)
	}
}

func TestAuthKeyFileRefusesLoosePermissions(t *testing.T) {
	t.Parallel()

	// A key readable by other users is a credential handed to them. Reading it
	// silently would be the wrong kind of convenient.
	dir := t.TempDir()
	path := filepath.Join(dir, "tailscale-auth-key")
	if err := os.WriteFile(path, []byte("tskey-secret\n"), 0o644); err != nil { //nolint:gosec // deliberately loose mode exercises rejection
		t.Fatal(err)
	}
	_, err := readTailscaleAuthKey(path)
	if err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("readTailscaleAuthKey() error = %v, want a permissions refusal", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := readTailscaleAuthKey(path)
	if err != nil {
		t.Fatalf("readTailscaleAuthKey() after chmod = %v", err)
	}
	if string(key) != "tskey-secret" {
		t.Fatalf("key = %q", key)
	}
}

func TestBootstrapPinResolutionRefusesDuplicateNamesAndAddresses(t *testing.T) {
	hosts := []cli.HostRecord{
		{ID: "owner-a", MeshIdentity: "owner-a", MachineName: "garden", Endpoint: "wss://first.example.test/control/ws", Addresses: []string{"100.64.0.7"}},
		{ID: "owner-b", MeshIdentity: "owner-b", MachineName: "garden", Endpoint: "wss://second.example.test/control/ws", Addresses: []string{"100.64.0.8"}},
	}
	if _, err := identityForTarget(hosts, "user@garden:2200"); err == nil {
		t.Fatal("duplicate owner name chose a bootstrap destination")
	}
	for _, target := range []string{"owner-a", "user@100.64.0.7:2200", "first.example.test"} {
		got, err := identityForTarget(hosts, target)
		if err != nil || got != "owner-a" {
			t.Fatalf("%s: %q, %v", target, got, err)
		}
	}
	hosts[1].Addresses = []string{"100.64.0.7"}
	if _, err := identityForTarget(hosts, "100.64.0.7"); err == nil {
		t.Fatal("duplicate address chose an identity")
	}
	hosts[0].MachineName = "owner-b"
	if got, err := identityForTarget(hosts, "owner-b"); err != nil || got != "owner-b" {
		t.Fatalf("exact identity did not precede name: %q, %v", got, err)
	}
}

func TestBootstrapKnownOwnerNameRequiresCurrentAuthenticatedDeclarationBeforeRunner(t *testing.T) {
	for _, mode := range []string{"available", "renamed", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			stateDir := isolateBootstrapState(t)
			client, _, err := identity.LoadOrCreate(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			owner, key, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			auth := &transport.Authentication{Key: key, Authorize: func(id string) bool { return id == client.ID }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
					frame, err := conn.ReadFrame()
					if err != nil {
						return fmt.Errorf("bootstrap declaration fixture: %w", err)
					}
					request, err := protocol.DecodeControl(frame.Payload)
					if err != nil {
						return fmt.Errorf("bootstrap declaration fixture: %w", err)
					}
					if request.Type != protocol.TypeHostInfo {
						return errors.New("bootstrap name preflight attempted an effect")
					}
					name, revision := "garden", uint64(1)
					if mode == "renamed" {
						name, revision = "owner-new", 2
					}
					payload, err := json.Marshal(protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{ID: owner.ID, MeshIdentity: owner.ID, MachineName: name, NameRevision: revision}})
					if err != nil {
						return fmt.Errorf("bootstrap declaration fixture: %w", err)
					}
					return conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
				})
			}))
			defer server.Close()
			host := cli.HostRecord{ID: owner.ID, MeshIdentity: owner.ID, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/control/ws"}
			if err := cli.SaveHost(host); err != nil {
				t.Fatal(err)
			}
			path, err := cli.ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := machinename.RememberClaim(t.Context(), filepath.Dir(path), owner.ID, machinename.Claim{ID: owner.ID, MachineName: "garden", Revision: 1}); err != nil {
				t.Fatal(err)
			}
			if mode == "unavailable" {
				server.Close()
			}
			ran := false
			run := newBootstrapFunc(func(context.Context, bootstrap.Options) (bootstrap.Result, error) {
				ran = true
				return bootstrap.Result{}, nil
			}, bootstrapUI{input: strings.NewReader(""), output: io.Discard})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, err = run(ctx, cli.AddRequest{Target: "garden"})
			if mode == "available" {
				if err != nil || !ran {
					t.Fatalf("known-good declaration did not reach runner: %v", err)
				}
				return
			}
			if err == nil || ran {
				t.Fatalf("%s retained name reached bootstrap runner: ran=%t err=%v", mode, ran, err)
			}
		})
	}
}
