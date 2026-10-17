package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transfer"
	"github.com/shaul/mesh/internal/transport"
)

func TestCopyEndpointRequiresAbsoluteRemotePath(t *testing.T) {
	tests := []struct {
		value string
		want  copyEndpoint
		bad   bool
	}{
		{value: "upcloud-4:/srv/artifact", want: copyEndpoint{host: "upcloud-4", path: "/srv/artifact"}},
		{value: "./file:name", want: copyEndpoint{path: "./file:name"}},
		{value: "/tmp/file:name", want: copyEndpoint{path: "/tmp/file:name"}},
		{value: "artifact", want: copyEndpoint{path: "artifact"}},
		{value: "upcloud-4:relative", bad: true},
		{value: ":/srv/artifact", bad: true},
	}
	for _, test := range tests {
		got, err := parseCopyEndpoint(test.value)
		if (err != nil) != test.bad || !test.bad && got != test.want {
			t.Errorf("parse %q = %+v, %v, want %+v, error %t", test.value, got, err, test.want, test.bad)
		}
	}
}

func TestCopyRequiresExactlyOneRemoteEndpoint(t *testing.T) {
	for _, args := range [][]string{{"local", "destination"}, {"pc:/source", "upcloud-4:/destination"}} {
		_, _, err := executeCommand(t, Dependencies{}, append([]string{"cp"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "requires one local path") {
			t.Fatalf("copy %v returned %v", args, err)
		}
	}
}

func TestCopyCommandTransfersFileInBothDirections(t *testing.T) {
	host := setupCommandTestHost(t)
	fixture := newCopyTestRemote(t, host.host)
	root := t.TempDir()
	local, remote, returned := filepath.Join(root, "local"), filepath.Join(root, "remote"), filepath.Join(root, "returned")
	writeCopyTestFile(t, local, "payload", 0o640)
	deps := Dependencies{DialControl: fixture.dial}
	if _, _, err := executeCommand(t, deps, "cp", local, "pc:"+remote); err != nil {
		t.Fatal(err)
	}
	assertCopyTestFile(t, remote, "payload", 0o640)
	if _, _, err := executeCommand(t, deps, "cp", "pc:"+remote, returned); err != nil {
		t.Fatal(err)
	}
	assertCopyTestFile(t, returned, "payload", 0o640)
	fixture.assertNoSessionControls(t)
}

func TestCopyCommandTransfersDirectoryInBothDirections(t *testing.T) {
	host := setupCommandTestHost(t)
	fixture := newCopyTestRemote(t, host.host)
	root := t.TempDir()
	local, remote, returned := filepath.Join(root, "source"), filepath.Join(root, "remote"), filepath.Join(root, "returned")
	if err := os.MkdirAll(filepath.Join(local, "nested", "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(local, ".hidden"), "dotfile", 0o600)
	writeCopyTestFile(t, filepath.Join(local, "nested", "script"), "echo hello\n", 0o750)
	deps := Dependencies{DialControl: fixture.dial}
	if _, _, err := executeCommand(t, deps, "cp", "-r", local, "pc:"+remote); err != nil {
		t.Fatal(err)
	}
	assertCopyTestTree(t, remote)
	if _, _, err := executeCommand(t, deps, "cp", "-r", "pc:"+remote, returned); err != nil {
		t.Fatal(err)
	}
	assertCopyTestTree(t, returned)
	fixture.assertNoSessionControls(t)
}

func TestCopyDirectoryRequiresRecursive(t *testing.T) {
	host := setupCommandTestHost(t)
	fixture := newCopyTestRemote(t, host.host)
	directory := t.TempDir()
	deps := Dependencies{DialControl: fixture.dial}
	for _, args := range [][]string{{directory, "pc:" + filepath.Join(t.TempDir(), "copy")}, {"pc:" + directory, filepath.Join(t.TempDir(), "copy")}} {
		_, _, err := executeCommand(t, deps, append([]string{"cp"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "recursive") {
			t.Fatalf("directory copy %v returned %v", args, err)
		}
	}
}

func TestCopyMalformedDownloadPreservesDestination(t *testing.T) {
	host := setupCommandTestHost(t)
	fixture := newCopyTestRemote(t, host.host)
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	writeCopyTestFile(t, source, "new", 0o644)
	writeCopyTestFile(t, destination, "old", 0o600)
	fixture.mutate = func(request transfer.Request, result *transfer.Result) {
		if request.Action == "download.chunk" && result.Done {
			result.Info.Digest = strings.Repeat("0", 64)
		}
	}
	_, _, err := executeCommand(t, Dependencies{DialControl: fixture.dial}, "cp", "pc:"+source, destination)
	if err == nil {
		t.Fatal("corrupt download succeeded")
	}
	assertCopyTestFile(t, destination, "old", 0o600)
	fixture.assertNoSessionControls(t)
}

func TestCopyInconsistentDownloadMetadataPreservesDestination(t *testing.T) {
	host := setupCommandTestHost(t)
	fixture := newCopyTestRemote(t, host.host)
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	writeCopyTestFile(t, source, "new", 0o644)
	writeCopyTestFile(t, destination, "old", 0o600)
	fixture.mutate = func(request transfer.Request, result *transfer.Result) {
		if request.Action == "download.chunk" && result.Done {
			result.Size++
		}
	}
	_, _, err := executeCommand(t, Dependencies{DialControl: fixture.dial}, "cp", "pc:"+source, destination)
	if err == nil || !strings.Contains(err.Error(), "inconsistent source metadata") {
		t.Fatalf("inconsistent download returned %v", err)
	}
	assertCopyTestFile(t, destination, "old", 0o600)
}

type copyTestRemote struct {
	host   HostRecord
	mu     sync.Mutex
	events []string
	mutate func(transfer.Request, *transfer.Result)
}

func newCopyTestRemote(t *testing.T, host HostRecord) *copyTestRemote {
	t.Helper()
	return &copyTestRemote{host: host}
}

func (r *copyTestRemote) dial(context.Context, HostRecord) (transport.Conn, error) {
	return &copyTestConn{remote: r, store: transfer.NewStore(), responses: make(chan protocol.Frame, 4), done: make(chan struct{})}, nil
}

func (r *copyTestRemote) assertNoSessionControls(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		t.Fatal("copy did not use the control connection")
	}
	for _, event := range r.events {
		if event != protocol.TypeHostInfo && event != protocol.TypeCopyRequest {
			t.Fatalf("copy sent unrelated control %q", event)
		}
	}
}

type copyTestConn struct {
	remote    *copyTestRemote
	store     *transfer.Store
	responses chan protocol.Frame
	done      chan struct{}
	close     sync.Once
}

func (c *copyTestConn) ReadFrame() (protocol.Frame, error) {
	select {
	case frame := <-c.responses:
		return frame, nil
	case <-c.done:
		return protocol.Frame{}, io.EOF
	}
}

func (c *copyTestConn) WriteFrame(frame protocol.Frame) error {
	if frame.Kind != protocol.KindControl {
		return errors.New("copy sent non-control frame")
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode fixture copy control: %w", err)
	}
	c.remote.mu.Lock()
	c.remote.events = append(c.remote.events, request.Type)
	c.remote.mu.Unlock()
	response, err := c.response(request)
	if err != nil {
		return err
	}
	c.responses <- mustCommandControlFrame(response)
	return nil
}

func (c *copyTestConn) response(request protocol.Control) (protocol.Control, error) {
	response := protocol.Control{RequestID: request.RequestID}
	if request.Type == protocol.TypeHostInfo {
		response.Type = protocol.TypeHostInfoResult
		response.Host = &protocol.HostInfo{ID: c.remote.host.ID, MeshIdentity: c.remote.host.MeshIdentity, MachineName: c.remote.host.MachineName, NameRevision: 1, TailscaleName: c.remote.host.TailscaleName}
		return response, nil
	}
	if request.Type != protocol.TypeCopyRequest {
		return response, fmt.Errorf("unexpected copy control %q", request.Type)
	}
	var payload transfer.Request
	if err := json.Unmarshal(request.Copy, &payload); err != nil {
		return response, fmt.Errorf("decode fixture copy payload: %w", err)
	}
	result, err := c.store.Handle(context.Background(), payload)
	if err != nil {
		response.Type, response.Message = protocol.TypeError, err.Error()
		return response, nil //nolint:nilerr // Return the encoded daemon error response to the client.
	}
	if c.remote.mutate != nil {
		c.remote.mutate(payload, &result)
	}
	response.Type = protocol.TypeCopyResult
	response.Copy, err = json.Marshal(result)
	if err != nil {
		return response, fmt.Errorf("encode fixture copy result: %w", err)
	}
	return response, nil
}

func (c *copyTestConn) Close() error {
	c.close.Do(func() { close(c.done) })
	if err := c.store.Close(); err != nil {
		return fmt.Errorf("close fixture copy store: %w", err)
	}
	return nil
}

func writeCopyTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil { //nolint:gosec // Write a fixture path selected by the test.
		t.Fatal(err)
	}
}

func assertCopyTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // Read the test-selected destination to verify copied contents.
	if err != nil || string(data) != contents {
		t.Fatalf("read %s = %q, %v, want %q", path, data, err, contents)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != mode {
		t.Fatalf("mode %s = %v, %v, want %v", path, info, err, mode)
	}
}

func assertCopyTestTree(t *testing.T, root string) {
	t.Helper()
	assertCopyTestFile(t, filepath.Join(root, ".hidden"), "dotfile", 0o600)
	assertCopyTestFile(t, filepath.Join(root, "nested", "script"), "echo hello\n", 0o750)
	info, err := os.Stat(filepath.Join(root, "nested", "empty"))
	if err != nil || !info.IsDir() {
		t.Fatalf("empty directory was not copied: %v", err)
	}
}
