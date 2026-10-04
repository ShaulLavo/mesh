package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/update"
	"golang.org/x/crypto/ssh"
)

func TestApprovalFixtureProcess(t *testing.T) {
	role := os.Getenv("MESH_175_FIXTURE_ROLE")
	if role == "" {
		t.Skip("fixture subprocess")
	}
	if role == "cli" {
		for index, arg := range os.Args {
			if arg != "--" {
				continue
			}
			command := deviceCommand()
			command.SetArgs(os.Args[index+2:])
			command.SilenceUsage, command.SilenceErrors = true, true
			if err := command.ExecuteContext(t.Context()); err != nil {
				_, _ = os.Stderr.WriteString(err.Error())
				os.Exit(1)
			}
			os.Exit(0)
		}
		os.Exit(2)
	}
	state := os.Getenv("MESH_175_SOCKET_STATE")
	host, err := identity.LoadOwned(state)
	if err != nil {
		t.Fatal(err)
	}
	if other := os.Getenv("MESH_175_REPORTED_ID"); other != "" {
		host.ID = other
	}
	listener, err := net.Listen("unix", filepath.Join(state, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	for {
		stream, err := listener.Accept()
		if err != nil {
			return
		}
		serveApprovalFixture(stream, host.ID)
	}
}

func serveApprovalFixture(stream net.Conn, id string) {
	defer func() { _ = stream.Close() }()
	frame, err := protocol.NewReader(stream).ReadFrame()
	if err != nil {
		return
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil || request.Type != protocol.TypeHostInfo {
		return
	}
	_ = protocol.NewWriter(stream).WriteControlMsg(protocol.Control{
		Type: protocol.TypeHostInfoResult, RequestID: request.RequestID,
		Host: &protocol.HostInfo{ID: id, MeshIdentity: id},
	})
}

func startApprovalFixture(t *testing.T, state, observed, reported string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestApprovalFixtureProcess$") //nolint:gosec // this test binary runs only the isolated Unix protocol fixture
	child.Env = append(os.Environ(), "MESH_175_FIXTURE_ROLE=daemon", "MESH_175_SOCKET_STATE="+state,
		"MESH_STATE_DIR="+observed, "MESH_175_REPORTED_ID="+reported)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(state, "daemon.sock")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture Unix daemon did not listen")
}

func approvalFixtureRequest(t *testing.T) checkedApproval {
	t.Helper()
	state, err := filepath.EvalSymlinks(testenv.SocketTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	destination, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return checkedApproval{Account: account.Username, StateDir: state, Destination: destination.ID, Source: source.ID, AllowRoot: true}
}

func TestCheckedApprovalNativeBindingAndIdempotency(t *testing.T) {
	request := approvalFixtureRequest(t)
	startApprovalFixture(t, request.StateDir, request.StateDir, "")
	before := snapshotApprovalFiles(t, request.StateDir)
	receipt, err := approveChecked(t.Context(), request)
	if err != nil || receipt.Approved {
		t.Fatalf("read-only preflight: %+v %v", receipt, err)
	}
	if before != snapshotApprovalFiles(t, request.StateDir) {
		t.Fatal("preflight wrote state")
	}
	request.Apply = true
	if receipt, err = approveChecked(t.Context(), request); err != nil || !receipt.Approved {
		t.Fatalf("native checked approval: %+v %v", receipt, err)
	}
	current, ok := identity.BindIdentity(request.StateDir, request.Source)
	if !ok {
		t.Fatal("source did not receive a full device grant on destination")
	}
	if _, err := approveChecked(t.Context(), request); err != nil || !current() {
		t.Fatalf("repeat approval changed the grant incarnation: %v", err)
	}
	t.Log("native Unix peer PID, process account/state, pinned daemon identity and repeated grant approval passed")
}

func TestCheckedApprovalRefusesBeforeWriting(t *testing.T) {
	cases := []string{"account", "pin", "state binding", "daemon identity", "missing state", "missing key", "unsafe key", "symlink key", "fifo key", "oversized key", "invalid key", "unsafe state", "root consent"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			request := approvalFixtureRequest(t)
			observed, reported := request.StateDir, ""
			other, _, err := identity.LoadOrCreate(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			start := true
			key := filepath.Join(request.StateDir, "identity.key")
			switch name {
			case "account":
				request.Account += "-wrong"
			case "pin":
				request.Destination = other.ID
			case "state binding":
				observed = t.TempDir()
			case "daemon identity":
				reported = other.ID
			case "missing state":
				request.StateDir = filepath.Join(request.StateDir, "missing")
				start = false
			case "missing key":
				requireApprovalMutation(t, os.Remove(key))
				start = false
			case "unsafe key":
				requireApprovalMutation(t, os.Chmod(key, 0644)) //nolint:gosec // deliberately unsafe key fixture must be rejected
				start = false
			case "symlink key":
				requireApprovalMutation(t, os.Rename(key, key+".saved"))
				requireApprovalMutation(t, os.Symlink(key+".saved", key))
				start = false
			case "fifo key":
				requireApprovalMutation(t, os.Remove(key))
				requireApprovalMutation(t, syscall.Mkfifo(key, 0600))
				start = false
			case "oversized key":
				requireApprovalMutation(t, os.WriteFile(key, []byte(strings.Repeat("x", (16<<10)+1)), 0600))
				start = false
			case "invalid key":
				requireApprovalMutation(t, os.WriteFile(key, []byte("invalid-owned-key"), 0600))
				start = false
			case "unsafe state":
				requireApprovalMutation(t, os.Chmod(request.StateDir, 0777)) //nolint:gosec // deliberately unsafe state fixture must be rejected
			case "root consent":
				if os.Geteuid() != 0 {
					t.Skip("root acknowledgement applies to root accounts")
				}
				request.AllowRoot = false
			}
			if start {
				startApprovalFixture(t, request.StateDir, observed, reported)
			}
			before := snapshotApprovalFiles(t, filepath.Dir(key))
			request.Apply = true
			if _, err := approveChecked(t.Context(), request); err == nil {
				t.Fatal("unsafe or mismatched destination accepted")
			}
			if before != snapshotApprovalFiles(t, filepath.Dir(key)) {
				t.Fatal("refused approval wrote state")
			}
		})
	}
}

func requireApprovalMutation(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func snapshotApprovalFiles(t *testing.T, root string) string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect fixture entry: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		contents, err := os.ReadFile(path) //nolint:gosec // private fixture tree is owned exclusively by this test
		if err != nil {
			return fmt.Errorf("read fixture entry: %w", err)
		}
		files[strings.TrimPrefix(path, root)] = string(contents)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	contents, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestCheckedApprovalEnvironmentRefusesUnavailableBinding(t *testing.T) {
	for _, environment := range [][]string{nil, {"MESH_STATE_DIR=relative"}, {"HOME=/one", "HOME=/two"}} {
		if _, err := approvalEnvironmentState(environment); err == nil {
			t.Fatal("unavailable or ambiguous state binding accepted")
		}
	}
	for _, environment := range [][]string{{"MESH_STATE_DIR=/state"}, {"XDG_STATE_HOME=/base"}, {"HOME=/home/account"}} {
		if _, err := approvalEnvironmentState(environment); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := approvalFixtureRequest(t)
	startApprovalFixture(t, request.StateDir, request.StateDir, "")
	if _, err := approveChecked(ctx, request); err == nil {
		t.Fatal("canceled preflight succeeded")
	}
}

func TestCheckedApprovalRestrictedKeysAndConcurrentWriters(t *testing.T) {
	request := approvalFixtureRequest(t)
	startApprovalFixture(t, request.StateDir, request.StateDir, "")
	other, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	requireApprovalMutation(t, identity.ApproveDevice(request.StateDir, other.ID))
	current, ok := identity.BindIdentity(request.StateDir, other.ID)
	if !ok {
		t.Fatal("unrelated grant did not bind")
	}
	key, err := identity.IdentityKey(request.Source)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(request.StateDir, "authorized_keys")
	original, err := os.ReadFile(path) //nolint:gosec // fixed authorized_keys in this test's private destination state
	if err != nil {
		t.Fatal(err)
	}
	restricted := append([]byte("command=\"fixture-only\" "), ssh.MarshalAuthorizedKey(public)...)
	requireApprovalMutation(t, os.WriteFile(path, append(original, restricted...), 0600)) //nolint:gosec // controlled restricted-key fixture in the private destination state
	requireApprovalMutation(t, update.Trust(request.StateDir, request.Source, false))
	updaterPath := filepath.Join(request.StateDir, "updates", "administrators.json")
	updaterBefore, err := os.ReadFile(updaterPath) //nolint:gosec // fixed updater policy in this test's private destination state
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := approveChecked(t.Context(), request)
	if err != nil || receipt.Approved {
		t.Fatalf("restricted/update-only key counted as a full device grant: %+v %v", receipt, err)
	}
	if !current() {
		t.Fatal("read-only preflight changed unrelated grant")
	}
	request.Apply = true
	results := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := approveChecked(t.Context(), request)
			results <- err
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	grants, err := identity.DeviceGrants(path)
	if err != nil || len(grants) != 2 || !current() {
		t.Fatalf("concurrent approval corrupted or replaced grants: %v %v", grants, err)
	}
	updaterAfter, err := os.ReadFile(updaterPath) //nolint:gosec // read back the same private fixture updater policy
	if err != nil || !bytes.Equal(updaterBefore, updaterAfter) {
		t.Fatalf("explicit full-device enrollment changed updater-only policy: %v", err)
	}
}
