package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

func TestPrivacySessionListsMaskMetadataKeepStatus(t *testing.T) {
	mask := privacy.New()
	rows := []protocol.SessionInfo{{ID: "7K3D", State: "running", Cwd: "/home/private-owner/secret-project", Label: "private-owner@private-host:~/secret-project", Command: []string{"/home/private-owner/bin/claude", "--resume", "f15946ad-d69f-45b3-845a-7317118b040a"}, CreatedAt: commandTestTime, MemoryBytes: 1024 * 1024, Recovery: &recovery.Record{Title: "secret-conversation"}}}
	for _, fleet := range []bool{false, true} {
		var output bytes.Buffer
		view := listView{privacy: mask}
		var err error
		if fleet {
			_, err = writeSessionList(&output, commandTestTime, []HostSessions{{Host: HostRecord{MachineName: "private-host"}, Sessions: rows}}, view)
		} else {
			_, err = writeLocalSessionList(&output, commandTestTime, rows, view)
		}
		if err != nil {
			t.Fatal(err)
		}
		text := output.String()
		for _, secret := range []string{"private-owner", "--resume", "f15946ad"} {
			if strings.Contains(text, secret) {
				t.Fatalf("leaked %q: %s", secret, text)
			}
		}
		for _, fact := range []string{"7K3D", "running", "1.0M", "claude", "private-host", "~/secret-project", mask.Value("path", rows[0].Cwd), mask.Value("title", rows[0].Label)} {
			if !strings.Contains(text, fact) {
				t.Fatalf("missing %q: %s", fact, text)
			}
		}
	}
	if rows[0].Cwd != "/home/private-owner/secret-project" || rows[0].Command[1] != "--resume" {
		t.Fatal("masking mutated source data")
	}
}

func TestPrivacyFlagEnvironmentAndOverride(t *testing.T) {
	for _, test := range []struct {
		name, environment string
		flags             []string
		hidden            bool
	}{
		{name: "flag", flags: []string{"--privacy"}, hidden: true},
		{name: "environment", environment: "1", hidden: true},
		{name: "override", environment: "1", flags: []string{"--privacy=false"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MESH_PRIVACY", test.environment)
			root := NewCommand(Dependencies{})
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			probe, _, findErr := root.Find([]string{"logs"})
			if findErr != nil {
				t.Fatal(findErr)
			}
			probe.RunE = func(cmd *cobra.Command, _ []string) error {
				_, _ = cmd.ErrOrStderr().Write([]byte("secret-bare-account"))
				return errors.New("secret-bare-account")
			}
			// Exercise the same wrapping used by every registered command.
			enabled := test.hidden
			protectPrivacyErrors(probe, &enabled)
			root.SetArgs(append(test.flags, "logs", "7K3D"))
			err := root.Execute()
			if err == nil {
				t.Fatal("expected probe error")
			}
			if test.hidden && strings.Contains(err.Error(), "secret-bare-account") {
				t.Fatal("returned error leaked secret")
			}
			if !test.hidden && !strings.Contains(output.String(), "secret-bare-account") {
				t.Fatal("override failed")
			}
		})
	}
}

func TestPrivacyLogsSuppressBothFormsBeforeReading(t *testing.T) {
	for _, previous := range []bool{false, true} {
		root := NewCommand(Dependencies{})
		var output bytes.Buffer
		root.SetOut(&output)
		args := []string{"--privacy", "logs", "7K3D"}
		if previous {
			args = append(args, "--previous")
		}
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if output.String() != "Terminal output hidden by privacy mode\n" {
			t.Fatalf("output = %q", output.String())
		}
	}
}

func TestPrivacyErrorsPreserveCauseAndHideArguments(t *testing.T) {
	root := NewCommand(Dependencies{})
	root.SetArgs([]string{"--privacy", "logs", "private-session", "private-extra"})
	err := root.Execute()
	if err == nil || strings.Contains(err.Error(), "private-") || !strings.Contains(err.Error(), "--privacy=false") {
		t.Fatalf("error = %v", err)
	}
	var hidden privacyError
	if !errors.As(err, &hidden) || errors.Unwrap(hidden) == nil {
		t.Fatal("error cause lost")
	}
}

func TestPrivacyKeepsTerminalAttachmentAvailable(t *testing.T) {
	a := &application{privacy: privacy.New()}
	if _, err := a.attachmentOptions(&cobra.Command{}, "", false); err != nil {
		t.Fatalf("privacy blocked attachment: %v", err)
	}
}

func TestPrivacyErrorRendererDoesNotUnwrapPrivateUsageDetails(t *testing.T) {
	err := privacyError{cause: &usageError{problem: "secret-host", example: "mesh add secret-host", details: []string{"private@example.test /home/private-owner"}}}
	var output bytes.Buffer
	RenderError(&output, fang.Styles{}, err)
	text := output.String()
	for _, secret := range []string{"secret-host", "private@example.test", "/home/private-owner"} {
		if strings.Contains(text, secret) {
			t.Fatalf("rendered error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "--privacy=false") {
		t.Fatal("privacy error disappeared")
	}
}

func TestPrivacyPreservesCreationAndAttachmentTargets(t *testing.T) {
	host := setupCommandTestHost(t)
	_, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial}, "--privacy", "pc", "--", "claude", "--resume", "private-conversation")
	if err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	created, attached := host.create, host.attach
	host.mu.Unlock()
	if strings.Join(created.Command, " ") != "claude --resume private-conversation" || attached.SessionID != "7K3D" {
		t.Fatalf("masked action input: create=%+v attach=%+v", created, attached)
	}
}

func TestPrivacyHostManagementPresentationPreservesRecords(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	raw := HostRecord{ID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", MachineName: "private-machine", NameRevision: 1, Endpoint: "ws://private-machine.example:7337/mesh"}
	output, _, err := executeCommand(t, Dependencies{Bootstrap: func(_ context.Context, request AddRequest) (BootstrapResult, error) {
		if request.Target != "private-user@private-machine" {
			t.Fatalf("masked bootstrap: %+v", request)
		}
		return BootstrapResult{Host: raw}, nil
	}}, "--privacy", "add", "private-user@private-machine")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-user", "private-khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"} {
		if strings.Contains(output, secret) {
			t.Fatalf("add leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, "added private-machine") {
		t.Fatalf("add lost its readable host name: %s", output)
	}
	hosts, err := LoadHosts()
	if err != nil || len(hosts) != 1 || hosts[0].MachineName != "private-machine" || hosts[0].Endpoint != raw.Endpoint {
		t.Fatalf("masked saved records: %+v, %v", hosts, err)
	}
	output, _, err = executeCommand(t, Dependencies{Wake: func(_ context.Context, host HostRecord) error {
		if host.MachineName != "private-machine" {
			t.Fatal("masked wake target")
		}
		return nil
	}}, "--privacy", "wake", raw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "woke private-machine") {
		t.Fatalf("wake output=%s", output)
	}
}

func TestPrivacyParserErrorsBeforeFlag(t *testing.T) {
	t.Setenv("MESH_PRIVACY", "")
	raw := errors.New("unknown flag: --private-secret-flag")
	for _, test := range []struct {
		args   []string
		hidden bool
	}{
		{[]string{"--private-secret-flag", "--privacy", "ls"}, true},
		{[]string{"--privacy", "--private-secret-flag"}, true},
		{[]string{"--privacy", "--privacy=false", "--private-secret-flag"}, false},
		{[]string{"local", "--", "--privacy", "--private-secret-flag"}, false},
	} {
		got := PrivacyErrorForArguments(raw, test.args)
		if strings.Contains(got.Error(), "private-secret-flag") == test.hidden {
			t.Fatalf("args=%v error=%s", test.args, got)
		}
	}
}

func TestPrivacyListDiagnosticMasksErrorKeepsStatus(t *testing.T) {
	setupCommandTestHost(t)
	_, stderr, err := executeCommand(t, Dependencies{DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("secret-bare-account /home/private-owner")
	}, DialControl: func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("secret-bare-account /home/private-owner")
	}}, "--privacy", "ls", "--timeout", "20ms")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "secret-bare-account") || strings.Contains(stderr, "/home/private-owner") || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("stderr=%s", stderr)
	}
}
