package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

func TestAgentPreservesNativeArgumentsAndExit(t *testing.T) {
	root := t.TempDir()
	provider := filepath.Join(root, "claude")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '<%s>\\n' \"$@\"\ncat\nprintf 'native stderr\\n' >&2\nexit 23\n"), 0o700); err != nil { //nolint:gosec // private executable fixture must be runnable by this test
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	command := NewCommand(Dependencies{})
	var output, diagnostics bytes.Buffer
	command.SetIn(strings.NewReader("native input\n"))
	command.SetOut(&output)
	command.SetErr(&diagnostics)
	command.SetArgs([]string{"agent", "claude", "--", "--unknown-provider-option", "spaces ; $(unexpanded)", ""})
	err := command.ExecuteContext(t.Context())
	if code, ok := StatusCode(err); !ok || code != 23 {
		t.Fatalf("native exit = %v", err)
	}
	want := "<--unknown-provider-option>\n<spaces ; $(unexpanded)>\n<>\nnative input\n"
	if output.String() != want || !strings.HasSuffix(diagnostics.String(), "native stderr\n") {
		t.Fatalf("native streams = %q / %q", output.String(), diagnostics.String())
	}
}

func TestAgentNativeSignalExitIsPreserved(t *testing.T) {
	command := &cobra.Command{}
	command.SetIn(strings.NewReader(""))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	err := runNativeAgent(command, "/bin/sh", []string{"-c", "kill -TERM $$"}, "", os.Environ())
	if code, ok := StatusCode(err); !ok || code != 143 {
		t.Fatalf("signal exit = %v", err)
	}
}

func TestAgentHelpDoesNotRunAProvider(t *testing.T) {
	command := NewCommand(Dependencies{})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"agent", "--help"})
	if err := command.ExecuteContext(t.Context()); err != nil || !strings.Contains(output.String(), "bind") || !strings.Contains(output.String(), "PROVIDER") {
		t.Fatalf("agent help = %q, %v", output.String(), err)
	}
}

func TestAgentLaunchLeasePreservesOriginalDirectoryAndReplacesToken(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "7K3D")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(directory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	finished := make(chan agentLeaseTestTraffic, 1)
	go serveAgentLeaseTest(listener, finished)
	command := &cobra.Command{}
	var output, diagnostics bytes.Buffer
	command.SetIn(strings.NewReader(""))
	command.SetOut(&output)
	command.SetErr(&diagnostics)
	launch := agentresume.Launch{Provider: agentresume.Claude, Executable: "/bin/sh", ProviderVersion: "fake", Directory: "/effective/provider/directory", DataRoot: "/unused"}
	err = runRegisteredAgent(command, worker.SessionWorkerLocation{SessionID: "7K3D", Dir: directory}, launch,
		[]string{"-c", "pwd; printf '%s\\n' \"$MESH_AGENT_TOKEN\""}, os.Environ(), "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != cwd+"\nlease-token\n" || diagnostics.Len() != 0 {
		t.Fatalf("registered provider = %q / %q", output.String(), diagnostics.String())
	}
	select {
	case traffic := <-finished:
		if traffic.begin.AgentLookupOnly || traffic.finish.Type != protocol.TypeAgentFinish || traffic.finish.AgentToken != "lease-token" {
			t.Fatalf("lease traffic = %#v", traffic)
		}
	case <-time.After(time.Second):
		t.Fatal("normal native exit did not finish its lease")
	}
}

type agentLeaseTestTraffic struct{ begin, finish protocol.Control }

func TestAgentLookupProofDoesNotClaimNativeResume(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "7K3D")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(directory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	finished := make(chan agentLeaseTestTraffic, 1)
	go serveAgentLeaseTest(listener, finished)
	lease, err := beginAgentInvocation(t.Context(), worker.SessionWorkerLocation{SessionID: "7K3D", Dir: directory},
		agentresume.Launch{Provider: agentresume.Codex}, "exact-id", true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.connection.Close() //nolint:errcheck // test lease
	if _, err := lease.finish(); err != nil {
		t.Fatal(err)
	}
	traffic := <-finished
	if !traffic.begin.AgentLookupOnly || !traffic.begin.AgentExplicit || traffic.begin.AgentExpectedID != "exact-id" {
		t.Fatalf("lookup provenance was lost: %#v", traffic.begin)
	}
}

func serveAgentLeaseTest(listener net.Listener, finished chan<- agentLeaseTestTraffic) {
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	request := readAgentTestControl(connection)
	_ = protocol.NewWriter(connection).WriteControlMsg(protocol.Control{Type: protocol.TypeContained, RequestID: request.RequestID,
		SessionID: "7K3D", ContainingSessions: []protocol.SessionIdentity{{HostID: "test-host", SessionID: "7K3D"}}})
	_ = connection.Close()
	connection, err = listener.Accept()
	if err != nil {
		return
	}
	defer connection.Close() //nolint:errcheck // test lease
	request = readAgentTestControl(connection)
	begin := request
	_ = protocol.NewWriter(connection).WriteControlMsg(protocol.Control{Type: protocol.TypeAgentBegun, RequestID: request.RequestID,
		SessionID: "7K3D", AgentHostID: "test-host", AgentToken: "lease-token"})
	request = readAgentTestControl(connection)
	finished <- agentLeaseTestTraffic{begin: begin, finish: request}
	_ = protocol.NewWriter(connection).WriteControlMsg(protocol.Control{Type: protocol.TypeOK, SessionID: "7K3D", AgentVerified: true})
}

func readAgentTestControl(connection net.Conn) protocol.Control {
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := protocol.NewReader(connection).ReadFrame()
	if err != nil {
		return protocol.Control{}
	}
	request, _ := protocol.DecodeControl(frame.Payload)
	return request
}

func TestAgentHookSilencesInvalidPayloadsAndMissingIntegration(t *testing.T) {
	t.Setenv("MESH_AGENT_TOKEN", "")
	for _, arguments := range [][]string{{"agent-hook"}, {"agent-hook", "unknown"}, {"agent-hook", "claude"}, {"agent-hook", "claude", "--unknown"}} {
		command := NewCommand(Dependencies{})
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetIn(strings.NewReader("not json"))
		command.SetArgs(arguments)
		if err := command.ExecuteContext(t.Context()); err != nil || output.Len() != 0 {
			t.Fatalf("hook %v leaked %q or error %v", arguments, output.String(), err)
		}
	}
}

func TestAgentHookRegistersExactIdentityWithoutPromptFields(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "7K3D")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(directory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan protocol.Control, 1)
	go receiveAgentHookTest(listener, requests)
	t.Setenv("MESH_AGENT_TOKEN", "invocation-token")
	t.Setenv("MESH_AGENT_SOCKET", paths.Socket(directory))
	t.Setenv("MESH_AGENT_SESSION_ID", "7K3D")
	t.Setenv("MESH_AGENT_HOST_ID", "host-one")
	command := NewCommand(Dependencies{})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetIn(strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"conversation-one","cwd":"/work/project","source":"startup","prompt":"do not persist this","transcript_path":"/private/transcript"}`))
	command.SetArgs([]string{"agent-hook", "claude"})
	if err := command.ExecuteContext(t.Context()); err != nil || output.Len() != 0 {
		t.Fatalf("hook output = %q, %v", output.String(), err)
	}
	select {
	case request := <-requests:
		if request.Type != protocol.TypeAgentEvent || request.AgentToken != "invocation-token" || request.AgentHostID != "host-one" || request.AgentEvent == nil || request.AgentEvent.ConversationID != "conversation-one" {
			t.Fatalf("registration = %#v", request)
		}
		encoded, _ := json.Marshal(request)
		if bytes.Contains(encoded, []byte("do not persist")) || bytes.Contains(encoded, []byte("transcript")) {
			t.Fatalf("registration retained unrelated provider fields: %s", encoded)
		}
	case <-time.After(time.Second):
		t.Fatal("hook did not register")
	}
}

func receiveAgentHookTest(listener net.Listener, requests chan<- protocol.Control) {
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	defer connection.Close() //nolint:errcheck // test server connection
	frame, err := protocol.NewReader(connection).ReadFrame()
	if err != nil {
		return
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return
	}
	requests <- request
	_ = protocol.NewWriter(connection).WriteControlMsg(protocol.Control{Type: protocol.TypeAgentRegistered, SessionID: request.SessionID, RequestID: request.RequestID})
}

func TestAgentInvocationEnvironmentReplacesInheritedRoute(t *testing.T) {
	lease := &agentLease{location: worker.SessionWorkerLocation{SessionID: "7K3D", Dir: "/work/state/s/7K3D"}, token: "new", hostID: "host-two"}
	got := agentInvocationEnv([]string{"PATH=/bin", "MESH_AGENT_TOKEN=old", "MESH_AGENT_SOCKET=/old", "MESH_AGENT_HOST_ID=old-host", "MESH_AGENT_SESSION_ID=ABCD"}, lease)
	want := []string{"PATH=/bin", "MESH_AGENT_TOKEN=new", "MESH_AGENT_SOCKET=/work/state/s/7K3D/sock", "MESH_AGENT_SESSION_ID=7K3D", "MESH_AGENT_HOST_ID=host-two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation environment = %v", got)
	}
}

func TestAgentHookSetupIsIdempotentAndPreservesUnrelatedHooks(t *testing.T) {
	fragment, err := agentresume.StableHookFragment(agentresume.Claude, "/work/bin/mesh's tool")
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"model":"retained-model","hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"unrelated-start"}]}],"Stop":[{"hooks":[{"type":"command","command":"unrelated-stop"}]}]}}`)
	installed, err := mergeAgentHooks(original, fragment, false)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := mergeAgentHooks(installed, fragment, false)
	if err != nil || !bytes.Equal(installed, repeated) {
		t.Fatalf("second installation changed settings: %s, %v", repeated, err)
	}
	uninstalled, err := mergeAgentHooks(installed, fragment, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"retained-model", "unrelated-start", "unrelated-stop", "startup"} {
		if !bytes.Contains(uninstalled, []byte(expected)) {
			t.Fatalf("uninstall lost %s: %s", expected, uninstalled)
		}
	}
	if bytes.Contains(uninstalled, []byte("agent-hook")) {
		t.Fatalf("uninstall left Mesh hooks: %s", uninstalled)
	}
}

func TestAgentSetupRejectsMalformedSettingsWithoutOverwriting(t *testing.T) {
	for _, current := range []string{"{", "null", `{"hooks":null}`, `{"hooks":{"SessionStart":[{"hooks":"invalid"}]}}`} {
		fragment, _ := agentresume.StableHookFragment(agentresume.Codex, "/work/bin/mesh")
		if _, err := mergeAgentHooks([]byte(current), fragment, false); err == nil {
			t.Fatalf("accepted malformed settings %s", current)
		}
	}
}

func TestAgentShellFunctionsPreserveArgumentsAndExistingFunction(t *testing.T) {
	mesh := filepath.Join(t.TempDir(), "mesh's helper")
	if err := os.WriteFile(mesh, []byte("#!/bin/sh\nprintf '<%s>\\n' \"$@\"\n"), 0o700); err != nil { //nolint:gosec // private executable fixture must be runnable by this test
		t.Fatal(err)
	}
	fragment := agentShellInit("bash", mesh)
	script := "function claude { printf 'existing function\\n'; }\n" + fragment + fragment + "codex 'one argument' '; $(literal)'\nclaude\n"
	command := exec.Command("/bin/bash", "--noprofile", "--norc", "-ic", script) //nolint:gosec // test executes its own generated shell integration against a private fake Mesh executable
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, io.Discard
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	want := "<agent>\n<codex>\n<-->\n<one argument>\n<; $(literal)>\nexisting function\n"
	if output.String() != want {
		t.Fatalf("shell wrapper changed invocation: %q", output.String())
	}
}
