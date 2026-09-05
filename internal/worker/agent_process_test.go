//go:build linux || darwin

package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func TestMain(m *testing.M) {
	role := os.Getenv("MESH_TEST_AGENT_ROLE")
	if role == "" {
		os.Exit(m.Run())
	}
	if err := runAgentTestProcess(role, os.Getenv("MESH_TEST_AGENT_DIR")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runAgentTestProcess(role, dir string) error {
	switch role {
	case "worker":
		env := agentProcessTestEnv("native", dir)
		_, err := Run(Config{ID: "7K3D", HostID: "test-host", Dir: dir, Cwd: dir,
			Command: []string{os.Args[0]}, Env: env, Cols: 80, Rows: 24})
		return err
	case "native":
		return runNativeAgentTestProcess(dir)
	case "reader":
		saved, err := recovery.Read(dir)
		if err != nil {
			return err
		}
		return writeRecoveredAgentTest(saved.Agent)
	default:
		return fmt.Errorf("unknown agent test process role")
	}
}

func writeRecoveredAgentTest(recipe *agentresume.Recipe) error {
	if recipe == nil {
		return fmt.Errorf("acknowledged identity was not saved")
	}
	argv, err := agentresume.ResumeCommand(*recipe)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Recipe agentresume.Recipe
		Argv   []string
	}{Recipe: *recipe, Argv: argv})
}

func agentProcessTestEnv(role, dir string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "MESH_TEST_AGENT_") {
			env = append(env, entry)
		}
	}
	return append(env, "MESH_TEST_AGENT_ROLE="+role, "MESH_TEST_AGENT_DIR="+dir)
}

func runNativeAgentTestProcess(dir string) error {
	lease, err := connectAgentTestWorker(paths.Socket(dir))
	if err != nil {
		return err
	}
	defer lease.Close() //nolint:errcheck // the test deliberately loses the worker
	launch := agentresume.Launch{Provider: agentresume.Claude, Executable: os.Args[0], ProviderVersion: "test-fixture",
		Directory: dir, DataRoot: dir, Options: []string{"--model", "fixture-model"}}
	response, err := exchangeAgentTestControl(lease, protocol.Control{Type: protocol.TypeAgentBegin,
		SessionID: "7K3D", AgentHostID: "test-host", AgentPID: os.Getpid(), AgentLaunch: &launch})
	if err != nil || response.Type != protocol.TypeAgentBegun {
		return fmt.Errorf("begin real invocation: %s: %v", response.Message, err)
	}
	if err := registerAgentTestIdentity(dir, response.AgentToken); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-acknowledged"), []byte("exact-durable-id"), 0o600); err != nil { //nolint:gosec // private test directory passed by this test's parent process
		return err
	}
	_, _ = io.Copy(io.Discard, lease)
	return nil
}

func registerAgentTestIdentity(dir, token string) error {
	conn, err := net.DialTimeout("unix", paths.Socket(dir), time.Second) //nolint:gosec // local worker socket in the test's private directory
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // bounded one-shot identity registration
	response, err := exchangeAgentTestControl(conn, protocol.Control{Type: protocol.TypeAgentEvent,
		SessionID: "7K3D", AgentHostID: "test-host", AgentToken: token, AgentProvider: agentresume.Claude,
		AgentEvent: &agentresume.Event{Kind: agentresume.Start, ConversationID: "exact-durable-id", Directory: dir}})
	if err != nil || response.Type != protocol.TypeAgentRegistered {
		return fmt.Errorf("register real invocation: %s: %v", response.Message, err)
	}
	return nil
}

func connectAgentTestWorker(socket string) (net.Conn, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond) //nolint:gosec // local worker socket supplied by the test's parent process
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func exchangeAgentTestControl(conn net.Conn, request protocol.Control) (protocol.Control, error) {
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetDeadline(time.Time{}) //nolint:errcheck // leave the invocation lease alive
	if err := protocol.NewWriter(conn).WriteControlMsg(request); err != nil {
		return protocol.Control{}, err
	}
	frame, err := protocol.NewReader(conn).ReadFrame()
	if err != nil {
		return protocol.Control{}, err
	}
	return protocol.DecodeControl(frame.Payload)
}

func TestAgentAcknowledgementSurvivesKilledWorkerAndFreshReader(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "7K3D")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "session-worker", "--id", "7K3D", "--dir", dir, "--") //nolint:gosec // re-execute this test binary with a private TestMain role
	command.Env = agentProcessTestEnv("worker", dir)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	ready := filepath.Join(dir, "agent-acknowledged")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if _, err := os.Stat(ready); err != nil {
		logs, _ := os.ReadFile(paths.Log(dir))
		t.Fatalf("real invocation was not acknowledged: %s %s", output.String(), logs)
	}
	reader := exec.Command(os.Args[0]) //nolint:gosec // fresh test process reads only the private checkpoint
	reader.Env = agentProcessTestEnv("reader", dir)
	contents, err := reader.Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Recipe agentresume.Recipe
		Argv   []string
	}
	if err := json.Unmarshal(contents, &result); err != nil {
		t.Fatal(err)
	}
	if result.Recipe.ConversationID != "exact-durable-id" || result.Recipe.Directory != dir || result.Recipe.Lifecycle != agentresume.Active || result.Recipe.InvocationToken == "" {
		t.Fatalf("fresh process recipe = %+v", result.Recipe)
	}
	if !slices.Contains(result.Argv, "--resume=exact-durable-id") {
		t.Fatalf("fresh process did not build exact native resume: %q", result.Argv)
	}
}
