package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
)

func TestBootstrapFuncPinsExistingIdentityAndMapsResult(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	stateDir := t.TempDir()
	t.Setenv("MESH_STATE_DIR", stateDir)
	identity := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := cli.SaveHost(cli.HostRecord{
		Alias: "pc", ID: identity, MeshIdentity: identity,
		Addresses: []string{"100.64.0.1"}, Endpoint: "ws://100.64.0.1:7337/mesh",
	}); err != nil {
		t.Fatal(err)
	}

	var captured bootstrap.Options
	var progress bytes.Buffer
	bootstrapFunc := newBootstrapFunc(func(_ context.Context, opts bootstrap.Options) (bootstrap.Result, error) {
		captured = opts
		opts.Progress(bootstrap.Event{Step: bootstrap.StepConnect, Detail: "alice@pc"})
		return bootstrap.Result{
			ID: identity, MeshIdentity: identity, TailscaleName: "pc.example.ts.net",
			TailscaleAddresses: []string{"100.64.0.2"}, Endpoint: "ws://100.64.0.2:7337/mesh",
			AlreadyConfigured: true,
		}, nil
	}, bootstrapUI{
		input: io.Reader(strings.NewReader("")), output: &progress,
		username: func() (string, error) { return "alice", nil },
	})

	result, err := bootstrapFunc(context.Background(), cli.AddRequest{Target: "pc", Alias: "pc"})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Target != "alice@pc" || captured.StateDir != stateDir || captured.ExpectedIdentity != identity {
		t.Fatalf("bootstrap options = %#v", captured)
	}
	if captured.SSH.Password == nil || captured.SSH.Passphrase == nil || captured.SSH.ConfirmHostKey == nil {
		t.Fatalf("SSH prompt callbacks = %#v, want all callbacks", captured.SSH)
	}
	if result.Host.ID != identity || result.Host.TailscaleName != "pc.example.ts.net" || result.Host.Endpoint != "ws://100.64.0.2:7337/mesh" || !result.AlreadyConfigured {
		t.Fatalf("bootstrap result = %#v", result)
	}
	if got := progress.String(); !strings.Contains(got, "connect") || !strings.Contains(got, "alice@pc") {
		t.Fatalf("progress = %q", got)
	}
}

func TestCommandDependenciesWireBootstrapAndPicker(t *testing.T) {
	dependencies := commandDependencies()
	if dependencies.Bootstrap == nil || dependencies.Picker == nil {
		t.Fatalf("command dependencies = %#v, want bootstrap and picker", dependencies)
	}
}

func TestTargetWithUserPreservesExplicitUser(t *testing.T) {
	called := false
	got, err := targetWithUser("bob@[fd7a::1]:2222", func() (string, error) {
		called = true
		return "", errors.New("must not be called")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "bob@[fd7a::1]:2222" || called {
		t.Fatalf("targetWithUser() = %q, lookup called = %t", got, called)
	}
}

func TestTargetWithUserNamesLookupFailure(t *testing.T) {
	_, err := targetWithUser("pc", func() (string, error) { return "", errors.New("not found") })
	if err == nil || !strings.Contains(err.Error(), "local SSH user") {
		t.Fatalf("targetWithUser() error = %v", err)
	}
}
