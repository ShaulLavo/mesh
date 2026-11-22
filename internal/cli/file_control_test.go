package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// fileControlRemote stands in for a host: it answers host.info and returns a
// canned result for each file control, recording the requests it saw.
type fileControlRemote struct {
	host    HostRecord
	files   bool
	replies map[string]protocol.Control
	mu      sync.Mutex
	seen    []protocol.Control
}

func (r *fileControlRemote) dial(context.Context, HostRecord) (transport.Conn, error) {
	return &fileControlConn{remote: r, responses: make(chan protocol.Frame, 4), done: make(chan struct{})}, nil
}

type fileControlConn struct {
	remote    *fileControlRemote
	responses chan protocol.Frame
	done      chan struct{}
	close     sync.Once
}

func (c *fileControlConn) ReadFrame() (protocol.Frame, error) {
	select {
	case frame := <-c.responses:
		return frame, nil
	case <-c.done:
		return protocol.Frame{}, errors.New("closed")
	}
}

func (c *fileControlConn) WriteFrame(frame protocol.Frame) error {
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err //nolint:wrapcheck // Test fixture.
	}
	c.remote.mu.Lock()
	c.remote.seen = append(c.remote.seen, request)
	c.remote.mu.Unlock()
	response := protocol.Control{RequestID: request.RequestID}
	if request.Type == protocol.TypeHostInfo {
		host := c.remote.host
		response.Type = protocol.TypeHostInfoResult
		response.Host = &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, MachineName: host.MachineName, NameRevision: 1, TailscaleName: host.TailscaleName, FilesSupported: c.remote.files}
	} else {
		response = c.remote.replies[request.Type]
		response.RequestID = request.RequestID
	}
	c.responses <- mustCommandControlFrame(response)
	return nil
}

func (c *fileControlConn) Close() error {
	c.close.Do(func() { close(c.done) })
	return nil
}

func (r *fileControlRemote) fileRequest(t *testing.T, requestType string) protocol.Control {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, request := range r.seen {
		if request.Type == requestType {
			return request
		}
	}
	t.Fatalf("host never saw %s", requestType)
	return protocol.Control{}
}

func newFileControlRemote(t *testing.T, files bool) (*fileControlRemote, Dependencies) {
	t.Helper()
	host := setupCommandTestHost(t)
	remote := &fileControlRemote{host: host.host, files: files, replies: map[string]protocol.Control{
		protocol.TypeFileRead:   fileReply(t, protocol.TypeFileReadResult, protocol.FileReadResult{Path: "/a", Encoding: "utf-8", Data: "hello", End: 5, Size: 5, EOF: true}),
		protocol.TypeFileList:   fileReply(t, protocol.TypeFileListResult, protocol.FileListResult{Path: "/", Entries: []protocol.FileEntry{{Name: "a", Kind: "file"}}}),
		protocol.TypeFileSearch: fileReply(t, protocol.TypeFileSearchResult, protocol.FileSearchResult{Path: "/", Matches: []protocol.FileMatch{{Path: "/a", Line: 1, Column: 1, Text: "hello"}}}),
		protocol.TypeFileWrite:  fileReply(t, protocol.TypeFileWriteResult, protocol.FileWriteResult{Path: "/a", Size: 5, Created: true}),
		protocol.TypeFilePatch:  fileReply(t, protocol.TypeFilePatchResult, protocol.FilePatchResult{Path: "/a", Size: 6, Edits: 2}),
	}}
	return remote, Dependencies{DialControl: remote.dial}
}

func TestFileControlCommandsAddressTheVerifiedHostAndPrintJSON(t *testing.T) {
	remote, deps := newFileControlRemote(t, true)
	source := filepath.Join(t.TempDir(), "contents")
	if err := os.WriteFile(source, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	for _, test := range []struct {
		args    []string
		request string
		want    string
	}{
		{[]string{"read", "/a", "--offset", "3", "--max-bytes", "10", "--digest"}, protocol.TypeFileRead, `"data": "hello"`},
		{[]string{"list", "/", "--limit", "5"}, protocol.TypeFileList, `"name": "a"`},
		{[]string{"search", "/", "hel+o", "--glob", "*.go", "--max-matches", "3"}, protocol.TypeFileSearch, `"text": "hello"`},
		{[]string{"write", "/a", "--file", source, "--create-only"}, protocol.TypeFileWrite, `"created": true`},
		{[]string{"patch", "/a", "--expected-sha256", digest, "--old", "a", "--new", "b", "--old", "c", "--new", "d"}, protocol.TypeFilePatch, `"edits": 2`},
	} {
		stdout, _, err := executeCommand(t, deps, append([]string{"file-control", "--host", "pc"}, test.args...)...)
		if err != nil || !strings.Contains(stdout, test.want) {
			t.Fatalf("%v: %q, %v", test.args, stdout, err)
		}
		var target struct {
			Target protocol.FileTarget `json:"target"`
		}
		request := remote.fileRequest(t, test.request)
		if err := json.Unmarshal(request.Files, &target); err != nil || target.Target.HostID != remote.host.ID {
			t.Fatalf("%s target %+v, %v", test.request, target.Target, err)
		}
	}
	var write protocol.FileWriteRequest
	if err := json.Unmarshal(remote.fileRequest(t, protocol.TypeFileWrite).Files, &write); err != nil || write.Data != "hello" || write.Encoding != protocol.FileEncodingUTF8 || !write.CreateOnly {
		t.Fatalf("write request: %+v, %v", write, err)
	}
	var patch protocol.FilePatchRequest
	if err := json.Unmarshal(remote.fileRequest(t, protocol.TypeFilePatch).Files, &patch); err != nil || len(patch.Edits) != 2 || patch.Edits[1].NewText != "d" {
		t.Fatalf("patch request: %+v, %v", patch, err)
	}
}

func TestFileControlCommandRefusesAHostWithoutFileControls(t *testing.T) {
	remote, deps := newFileControlRemote(t, false)
	_, _, err := executeCommand(t, deps, "file-control", "--host", "pc", "read", "/a")
	if err == nil || !strings.Contains(err.Error(), "does not serve file controls") {
		t.Fatalf("host without file controls: %v", err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for _, request := range remote.seen {
		if protocol.IsFileControl(request.Type) {
			t.Fatalf("sent %s to a host that does not serve it", request.Type)
		}
	}
}

func TestFileControlCommandChecksItsArguments(t *testing.T) {
	_, deps := newFileControlRemote(t, true)
	if _, _, err := executeCommand(t, deps, "file-control", "read", "/a"); err == nil {
		t.Fatal("a missing host was accepted")
	}
	if _, _, err := executeCommand(t, deps, "file-control", "--host", "pc", "patch", "/a", "--old", "a"); err == nil {
		t.Fatal("an edit without a replacement was accepted")
	}
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, make([]byte, protocol.MaxFileWrite+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, deps, "file-control", "--host", "pc", "write", "/a", "--file", big, "--create-only"); err == nil {
		t.Fatal("an oversized source was accepted")
	}
}

func TestLoadWriteDataPicksTheExactEncoding(t *testing.T) {
	var request protocol.FileWriteRequest
	if err := loadWriteData(strings.NewReader("héllo"), "-", &request); err != nil || request.Encoding != protocol.FileEncodingUTF8 || request.Data != "héllo" {
		t.Fatalf("text: %+v %v", request, err)
	}
	if err := loadWriteData(strings.NewReader("\xff\xfe"), "-", &request); err != nil || request.Encoding != protocol.FileEncodingBase64 || request.Data != "//4=" {
		t.Fatalf("binary: %+v %v", request, err)
	}
}

func TestLoadWriteDataSendsEscapeHeavyTextAsBase64(t *testing.T) {
	var request protocol.FileWriteRequest
	markup := strings.Repeat("<", protocol.MaxFileWrite)
	if err := loadWriteData(strings.NewReader(markup), "-", &request); err != nil || request.Encoding != protocol.FileEncodingBase64 {
		t.Fatalf("escape-heavy text: %s %v", request.Encoding, err)
	}
}
