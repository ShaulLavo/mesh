package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// scriptedFileConn answers each request with the next canned control and keeps
// what it was sent.
// exchangeOnConn runs requests one at a time on conn.
func exchangeOnConn(conn transport.Conn) FileExchange {
	return func(ctx context.Context, request protocol.Control) (protocol.Control, error) {
		return controlRequest(ctx, conn, request)
	}
}

type scriptedFileConn struct {
	replies []protocol.Control
	sent    []protocol.Control
	pending []protocol.Frame
}

func (c *scriptedFileConn) WriteFrame(frame protocol.Frame) error {
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	c.sent = append(c.sent, request)
	reply := c.replies[0]
	c.replies = c.replies[1:]
	if reply.RequestID == "" {
		reply.RequestID = request.RequestID
	}
	payload, err := reply.Encode()
	if err != nil {
		return fmt.Errorf("encode reply: %w", err)
	}
	c.pending = append(c.pending, protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	return nil
}

func (c *scriptedFileConn) ReadFrame() (protocol.Frame, error) {
	if len(c.pending) == 0 {
		return protocol.Frame{}, io.EOF
	}
	frame := c.pending[0]
	c.pending = c.pending[1:]
	return frame, nil
}

func (c *scriptedFileConn) Close() error { return nil }

func fileReply(t *testing.T, replyType string, document any) protocol.Control {
	t.Helper()
	raw, err := protocol.EncodeFileDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Control{Type: replyType, Files: raw}
}

var testFileTarget = protocol.FileTarget{HostID: "HOST", Account: "shaul"}

func TestHostFilesAddressesTheExactTargetAndDecodesResults(t *testing.T) {
	conn := &scriptedFileConn{replies: []protocol.Control{
		fileReply(t, protocol.TypeFileReadResult, protocol.FileReadResult{Target: testFileTarget, Path: "/a", Encoding: "utf-8", Data: "hi", End: 2, Size: 2, EOF: true}),
		fileReply(t, protocol.TypeFileListResult, protocol.FileListResult{Target: testFileTarget, Path: "/", Entries: []protocol.FileEntry{{Name: "a", Kind: "file"}}}),
		fileReply(t, protocol.TypeFileSearchResult, protocol.FileSearchResult{Target: testFileTarget, Path: "/", Matches: []protocol.FileMatch{{Path: "/a", Line: 1, Column: 1, Text: "hi"}}}),
		fileReply(t, protocol.TypeFileWriteResult, protocol.FileWriteResult{Target: testFileTarget, Path: "/a", Size: 2, Created: true}),
		fileReply(t, protocol.TypeFilePatchResult, protocol.FilePatchResult{Target: testFileTarget, Path: "/a", Size: 3, Edits: 1}),
	}}
	files := HostFiles{Exchange: exchangeOnConn(conn), Target: testFileTarget}
	ctx := t.Context()
	// A target the caller put in the request is replaced by the helper's.
	read, err := files.Read(ctx, protocol.FileReadRequest{Target: protocol.FileTarget{HostID: "OTHER"}, Path: "/a"})
	if err != nil || read.Data != "hi" || !read.EOF {
		t.Fatalf("read: %+v %v", read, err)
	}
	if list, err := files.List(ctx, protocol.FileListRequest{Path: "/"}); err != nil || len(list.Entries) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if search, err := files.Search(ctx, protocol.FileSearchRequest{Path: "/", Pattern: "hi"}); err != nil || len(search.Matches) != 1 {
		t.Fatalf("search: %+v %v", search, err)
	}
	if write, err := files.Write(ctx, protocol.FileWriteRequest{Path: "/a", Data: "hi", CreateOnly: true}); err != nil || !write.Created {
		t.Fatalf("write: %+v %v", write, err)
	}
	if patch, err := files.Patch(ctx, protocol.FilePatchRequest{Path: "/a", Edits: []protocol.FileEdit{{OldText: "a", NewText: "b"}}}); err != nil || patch.Edits != 1 {
		t.Fatalf("patch: %+v %v", patch, err)
	}
	wantTypes := []string{protocol.TypeFileRead, protocol.TypeFileList, protocol.TypeFileSearch, protocol.TypeFileWrite, protocol.TypeFilePatch}
	for i, request := range conn.sent {
		if request.Type != wantTypes[i] {
			t.Fatalf("request %d is %s", i, request.Type)
		}
		var target struct {
			Target protocol.FileTarget `json:"target"`
		}
		if err := json.Unmarshal(request.Files, &target); err != nil || target.Target != testFileTarget {
			t.Fatalf("request %d target %+v, %v", i, target.Target, err)
		}
	}
}

func TestHostFilesKeepsTheDaemonsErrorCode(t *testing.T) {
	for code, want := range map[string]agentapi.Code{
		"file.conflict":          agentapi.FileConflict,
		"file.changed":           agentapi.FileChanged,
		"file.not_regular":       agentapi.FileNotRegular,
		"host.identity_mismatch": agentapi.HostIdentityMismatch,
		"file.target_mismatch":   agentapi.FileTargetMismatch,
		"resource.limit":         agentapi.ResourceLimit,
	} {
		conn := &scriptedFileConn{replies: []protocol.Control{{Type: protocol.TypeError, ErrorCode: code, Message: "refused"}}}
		_, err := HostFiles{Exchange: exchangeOnConn(conn), Target: testFileTarget}.Write(t.Context(), protocol.FileWriteRequest{Path: "/a", Data: "x", CreateOnly: true})
		var control *ControlError
		if !errors.As(err, &control) || control.Code != code {
			t.Fatalf("%s: %v", code, err)
		}
		if got := controlAgentCode(control.Code); got != want {
			t.Fatalf("%s maps to %s, want %s", code, got, want)
		}
	}
}

func TestHostFilesRefusesBadReplies(t *testing.T) {
	conn := &scriptedFileConn{replies: []protocol.Control{fileReply(t, protocol.TypeFileListResult, protocol.FileListResult{})}}
	if _, err := (HostFiles{Exchange: exchangeOnConn(conn), Target: testFileTarget}).Read(t.Context(), protocol.FileReadRequest{Path: "/a"}); err == nil {
		t.Fatal("a result of the wrong type was accepted")
	}
	conn = &scriptedFileConn{replies: []protocol.Control{{Type: protocol.TypeFileReadResult, Files: []byte(`{"path":1}`)}}}
	if _, err := (HostFiles{Exchange: exchangeOnConn(conn), Target: testFileTarget}).Read(t.Context(), protocol.FileReadRequest{Path: "/a"}); err == nil {
		t.Fatal("a malformed result was accepted")
	}
	if _, err := (HostFiles{Target: testFileTarget}).Read(t.Context(), protocol.FileReadRequest{Path: "/a"}); err == nil {
		t.Fatal("a missing connection was accepted")
	}
}

func TestHostFilesDoesNotSendWhatCannotFitOneFrame(t *testing.T) {
	conn := &scriptedFileConn{}
	_, err := HostFiles{Exchange: exchangeOnConn(conn), Target: testFileTarget}.Write(t.Context(), protocol.FileWriteRequest{Path: "/a", Data: strings.Repeat("x", protocol.MaxPayload), CreateOnly: true})
	var control *ControlError
	if !errors.As(err, &control) || control.Code != "resource.limit" || len(conn.sent) != 0 {
		t.Fatalf("oversized request: %v, sent %d", err, len(conn.sent))
	}
}

func TestFileControlsSupportedFollowsHostInfo(t *testing.T) {
	if err := FileControlsSupported(protocol.HostInfo{FilesSupported: true}); err != nil {
		t.Fatal(err)
	}
	err := FileControlsSupported(protocol.HostInfo{})
	var control *ControlError
	if !errors.As(err, &control) || controlAgentCode(control.Code) != agentapi.ProtocolUnsupported {
		t.Fatalf("unsupported host: %v", err)
	}
}
