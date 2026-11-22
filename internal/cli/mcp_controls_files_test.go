package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

const (
	filesTestHostID   = "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	filesTestContext  = "0123456789abcdef0123456789abcdef"
	filesTestCallerID = protocol.CallerCredential("test-credential")
)

// fileHostFixture is a remote host whose daemon answers host.info and the
// file controls from a handler, recording every file request it received.
type fileHostFixture struct {
	t        *testing.T
	host     HostRecord
	controls *mcpControls
	mu       sync.Mutex
	files    bool
	account  string
	seen     []protocol.Control
	respond  func(protocol.Control) (protocol.Control, error)
}

func newFileHostFixture(t *testing.T) *fileHostFixture {
	t.Helper()
	prepareCommandEnvironment(t)
	f := &fileHostFixture{t: t, files: true, account: "remote-bob"}
	f.host = HostRecord{ID: filesTestHostID, MeshIdentity: filesTestHostID, MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	dial := func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				f.mu.Lock()
				defer f.mu.Unlock()
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, Account: f.account, FilesSupported: f.files}}, nil
			}
			f.mu.Lock()
			f.seen = append(f.seen, req)
			respond := f.respond
			f.mu.Unlock()
			if respond == nil {
				return protocol.Control{}, errors.New("no scripted response")
			}
			return respond(req)
		}), nil
	}
	pool := newMCPHostPool(dial, time.Now)
	t.Cleanup(pool.Close)
	app := &application{dependencies: Dependencies{Now: time.Now}}
	f.controls = newMCPControls(app, pool, "")
	return f
}

func (f *fileHostFixture) requests() []protocol.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Control(nil), f.seen...)
}

func fileResult(t *testing.T, resultType string, document any) protocol.Control {
	t.Helper()
	raw, err := protocol.EncodeFileDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Control{Type: resultType, Files: raw}
}

func requestTarget(t *testing.T, request protocol.Control) protocol.FileTarget {
	t.Helper()
	var body struct {
		Target protocol.FileTarget `json:"target"`
	}
	if err := json.Unmarshal(request.Files, &body); err != nil {
		t.Fatal(err)
	}
	return body.Target
}

func TestMCPFileReadAddressesTheExactHostAccountAndReportsTheWorkspace(t *testing.T) {
	f := newFileHostFixture(t)
	f.controls.caller = filesTestCallerID
	handle := mcpnative.ContextRef{HostID: filesTestHostID, ContextID: filesTestContext}
	f.respond = func(req protocol.Control) (protocol.Control, error) {
		return fileResult(t, protocol.TypeFileReadResult, protocol.FileReadResult{
			Target: protocol.FileTarget{HostID: filesTestHostID, Account: "remote-bob", ContextID: filesTestContext, Cwd: "/work/app"},
			Path:   "/work/app/a.txt", Encoding: "utf-8", Data: "alpha\n", Start: 0, End: 6, Size: 6, EOF: true, Revision: "rev1", SHA256: strings.Repeat("a", 64),
		}), nil
	}
	out, err := f.controls.FileRead(t.Context(), mcpnative.FileReadRequest{Target: mcpnative.FileTargetRef{Context: &handle}, Path: "a.txt", Timeout: 5 * time.Second})
	if err != nil || out.IsError {
		t.Fatalf("read: %+v %v", out, err)
	}
	requests := f.requests()
	if len(requests) != 1 || requests[0].Type != protocol.TypeFileRead || requests[0].Caller != filesTestCallerID {
		t.Fatalf("requests: %+v", requests)
	}
	if target := requestTarget(t, requests[0]); target.HostID != filesTestHostID || target.Account != "remote-bob" || target.ContextID != filesTestContext {
		t.Fatalf("request target: %+v", target)
	}
	data, ok := out.Envelope.Data.(mcpnative.FileReadData)
	if !ok || data.Start != "0" || data.End != "6" || data.Size != "6" || !data.EOF || data.Data != "alpha\n" {
		t.Fatalf("data: %#v", out.Envelope.Data)
	}
	want := mcpnative.TargetMeta{HostID: filesTestHostID, HostName: "pc", Account: "remote-bob", ContextID: handle.String(), Cwd: "/work/app"}
	if out.Target == nil || *out.Target != want || data.Target != want {
		t.Fatalf("target: %+v, data target %+v, want %+v", out.Target, data.Target, want)
	}
}

func TestMCPFileToolsRefuseUnsafeTargetsBeforeSending(t *testing.T) {
	f := newFileHostFixture(t)
	handle := mcpnative.ContextRef{HostID: filesTestHostID, ContextID: filesTestContext}
	read := func(target mcpnative.FileTargetRef) mcpnative.CallOutcome {
		out, err := f.controls.FileRead(t.Context(), mcpnative.FileReadRequest{Target: target, Path: "a.txt", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// A context handle needs the application identity.
	if out := read(mcpnative.FileTargetRef{Context: &handle}); !out.IsError || out.Envelope.Errors[0].Code != agentapi.UsageInvalid {
		t.Fatalf("no identity: %+v", out)
	}
	// A context from another host than the named one is refused.
	f.controls.caller = filesTestCallerID
	other := mcpnative.ContextRef{HostID: "other-host-id-other-host-id-other-host-id1", ContextID: filesTestContext}
	if out := read(mcpnative.FileTargetRef{Host: filesTestHostID, Context: &other}); !out.IsError || out.Envelope.Errors[0].Code != agentapi.UsageInvalid {
		t.Fatalf("context of another host: %+v", out)
	}
	// A host whose daemon predates file controls is protocol.unsupported.
	f.files = false
	if out := read(mcpnative.FileTargetRef{Host: filesTestHostID}); !out.IsError || out.Envelope.Errors[0].Code != agentapi.ProtocolUnsupported {
		t.Fatalf("unsupported host: %+v", out)
	}
	if len(f.requests()) != 0 {
		t.Fatalf("a refused call sent %d requests", len(f.requests()))
	}
}

func TestMCPFileDaemonErrorsKeepTheirCodes(t *testing.T) {
	f := newFileHostFixture(t)
	for code, want := range map[string]agentapi.Code{
		"file.conflict": agentapi.FileConflict, "file.changed": agentapi.FileChanged, "file.not_regular": agentapi.FileNotRegular,
		"file.patch_mismatch": agentapi.FilePatchMismatch, "file.cursor_invalid": agentapi.FileCursorInvalid,
		"file.target_mismatch": agentapi.FileTargetMismatch, "context.closed": agentapi.ContextClosed, "context.not_found": agentapi.ContextNotFound,
	} {
		f.respond = func(protocol.Control) (protocol.Control, error) {
			return protocol.Control{Type: protocol.TypeError, ErrorCode: code, Message: "refused"}, nil
		}
		out, err := f.controls.FileWrite(t.Context(), mcpnative.FileWriteRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/a", Data: []byte("x"), CreateOnly: true, Timeout: 5 * time.Second})
		if err != nil || !out.IsError || out.Envelope.Errors[0].Code != want {
			t.Fatalf("%s: %+v %v", code, out, err)
		}
	}
}

func TestMCPFileWriteChoosesEncodingAndReportsUnknownOutcome(t *testing.T) {
	f := newFileHostFixture(t)
	var last protocol.FileWriteRequest
	f.respond = func(req protocol.Control) (protocol.Control, error) {
		if err := protocol.DecodeFileDocument(req.Files, &last); err != nil {
			return protocol.Control{}, fmt.Errorf("decode write request: %w", err)
		}
		return fileResult(t, protocol.TypeFileWriteResult, protocol.FileWriteResult{Target: protocol.FileTarget{HostID: filesTestHostID, Account: "remote-bob"}, Path: last.Path, Size: 3, SHA256: strings.Repeat("c", 64), Revision: "rev", Created: last.CreateOnly}), nil
	}
	write := func(data []byte) {
		out, err := f.controls.FileWrite(t.Context(), mcpnative.FileWriteRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/a", Data: data, CreateOnly: true, Timeout: 5 * time.Second})
		if err != nil || out.IsError {
			t.Fatalf("write: %+v %v", out, err)
		}
	}
	write([]byte("héllo"))
	if last.Encoding != protocol.FileEncodingUTF8 || last.Data != "héllo" {
		t.Fatalf("text: %+v", last)
	}
	write([]byte{0xff, 0xfe, 0})
	if last.Encoding != protocol.FileEncodingBase64 || last.Data != "//4A" {
		t.Fatalf("binary: %+v", last)
	}
	write([]byte(strings.Repeat("<", protocol.MaxFileWrite)))
	if last.Encoding != protocol.FileEncodingBase64 {
		t.Fatal("escape-heavy text must travel as Base64")
	}
	// A connection lost after the request was sent leaves the outcome unknown.
	f.respond = func(protocol.Control) (protocol.Control, error) {
		return protocol.Control{}, errors.New("connection reset")
	}
	out, err := f.controls.FileWrite(t.Context(), mcpnative.FileWriteRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/a", Data: []byte("x"), CreateOnly: true, Timeout: 5 * time.Second})
	if err != nil || !out.IsError || out.Envelope.Errors[0].Code != agentapi.RequestOutcomeUnknown {
		t.Fatalf("lost reply: %+v %v", out, err)
	}
	if got := len(f.requests()); got != 4 {
		t.Fatalf("a failed write was replayed: %d requests", got)
	}
}

func TestMCPFileListSearchAndPatchMapResults(t *testing.T) {
	f := newFileHostFixture(t)
	ran := protocol.FileTarget{HostID: filesTestHostID, Account: "remote-bob"}
	f.respond = func(req protocol.Control) (protocol.Control, error) {
		switch req.Type {
		case protocol.TypeFileList:
			return fileResult(t, protocol.TypeFileListResult, protocol.FileListResult{Target: ran, Path: "/d", Entries: []protocol.FileEntry{{Name: "a", Kind: "file", Size: 7, Mode: "0644"}, {Name: "s", Kind: "dir"}}, NextCursor: "next"}), nil
		case protocol.TypeFileSearch:
			return fileResult(t, protocol.TypeFileSearchResult, protocol.FileSearchResult{Target: ran, Path: "/d", Matches: []protocol.FileMatch{{Path: "/d/a", Line: 2, Column: 3, Text: "hit"}}, FilesVisited: 4, DirectoriesVisited: 1, Skipped: 2, Truncated: "matches_limit"}), nil
		default:
			return fileResult(t, protocol.TypeFilePatchResult, protocol.FilePatchResult{Target: ran, Path: "/d/a", Size: 9, SHA256: strings.Repeat("d", 64), Revision: "rev", Edits: 2}), nil
		}
	}
	list, _ := f.controls.FileList(t.Context(), mcpnative.FileListRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/d", Timeout: 5 * time.Second})
	entries := list.Envelope.Data.(mcpnative.FileListData)
	if len(entries.Entries) != 2 || entries.Entries[0].Size != "7" || entries.Entries[1].Size != "" || entries.NextCursor != "next" {
		t.Fatalf("list: %+v", entries)
	}
	search, _ := f.controls.FileSearch(t.Context(), mcpnative.FileSearchRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/d", Pattern: "hit", Timeout: 5 * time.Second})
	found := search.Envelope.Data.(mcpnative.FileSearchData)
	if len(found.Matches) != 1 || found.Matches[0].Line != 2 || found.Truncated != "matches_limit" || found.DirectoriesVisited != 1 {
		t.Fatalf("search: %+v", found)
	}
	patch, _ := f.controls.FilePatch(t.Context(), mcpnative.FilePatchRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/d/a", ExpectedSHA256: strings.Repeat("d", 64), Edits: []mcpnative.FileEditRequest{{OldText: "a", NewText: "b"}}, Timeout: 5 * time.Second})
	patched := patch.Envelope.Data.(mcpnative.FileWriteData)
	if patched.Edits != 2 || patched.Size != "9" || patched.Revision != "rev" {
		t.Fatalf("patch: %+v", patched)
	}
}

func TestMCPCopyTransfersBothWaysAndRefusesTheLocalHost(t *testing.T) {
	host := setupCommandTestHost(t)
	remote := newCopyTestRemote(t, host.host)
	app := &application{dependencies: Dependencies{Now: time.Now, DialControl: remote.dial}}
	pool := newMCPHostPool(func(context.Context, HostRecord) (transport.Conn, error) { return nil, errors.New("unused") }, time.Now)
	defer pool.Close()
	controls := newMCPControls(app, pool, "")
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	remotePath := filepath.Join(root, "remote.txt")
	returned := filepath.Join(root, "returned.txt")
	writeCopyTestFile(t, source, "payload", 0o640)

	up, err := controls.Copy(t.Context(), mcpnative.CopyRequest{Host: filesTestHostID, Direction: mcpnative.CopyUpload, LocalPath: source, RemotePath: remotePath, Timeout: 10 * time.Second})
	if err != nil || up.IsError {
		t.Fatalf("upload: %+v %v", up, err)
	}
	assertCopyTestFile(t, remotePath, "payload", 0o640)
	data := up.Envelope.Data.(mcpnative.CopyData)
	if data.Direction != mcpnative.CopyUpload || data.Target.HostID != host.host.ID || data.Bytes == "0" || data.Bytes == "" {
		t.Fatalf("upload data: %+v", data)
	}
	down, err := controls.Copy(t.Context(), mcpnative.CopyRequest{Host: filesTestHostID, Direction: mcpnative.CopyDownload, LocalPath: returned, RemotePath: remotePath, Timeout: 10 * time.Second})
	if err != nil || down.IsError {
		t.Fatalf("download: %+v %v", down, err)
	}
	assertCopyTestFile(t, returned, "payload", 0o640)

	// A missing source is a failed call with a code, and nothing is created.
	missing, err := controls.Copy(t.Context(), mcpnative.CopyRequest{Host: filesTestHostID, Direction: mcpnative.CopyUpload, LocalPath: filepath.Join(root, "absent"), RemotePath: filepath.Join(root, "x"), Timeout: 10 * time.Second})
	if err != nil || !missing.IsError {
		t.Fatalf("missing source: %+v %v", missing, err)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Fatal("a failed upload created the destination")
	}
}

func openProfileTestState(t *testing.T) *mcpControls {
	t.Helper()
	state, config := t.TempDir(), t.TempDir()
	t.Setenv("MESH_STATE_DIR", state)
	t.Setenv("MESH_CONFIG_DIR", config)
	if _, _, err := identity.LoadOrCreate(state); err != nil {
		t.Fatal(err)
	}
	pool := newMCPHostPool(nil, time.Now)
	t.Cleanup(pool.Close)
	return newMCPControls(&application{dependencies: Dependencies{Now: time.Now}}, pool, "")
}

func TestMCPProfileSetNeedsTheCurrentRevision(t *testing.T) {
	controls := openProfileTestState(t)
	get, err := controls.ProfileGet(t.Context(), mcpnative.ProfileGetRequest{Scope: mcpnative.ProfileScopeHost})
	section := get.Envelope.Data.(agentapi.ProfileSection)
	if err != nil || get.IsError || section.State != "absent" || section.Revision != nil && *section.Revision != 0 {
		t.Fatalf("absent profile: %+v %v", get, err)
	}
	if get.Target == nil || get.Target.HostID == "" {
		t.Fatalf("profile target: %+v", get.Target)
	}
	set := func(expected uint64, document string) mcpnative.CallOutcome {
		out, err := controls.ProfileSet(t.Context(), mcpnative.ProfileSetRequest{Scope: mcpnative.ProfileScopeHost, ExpectedRevision: expected, Document: []byte(document)})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := set(0, `{"schemaVersion":1,"description":"build box","instructions":"use the big disk"}`)
	if first.IsError || first.Envelope.Data.(agentapi.ProfileWrite).Revision != 1 {
		t.Fatalf("first set: %+v", first)
	}
	// A stale revision conflicts and changes nothing.
	stale := set(0, `{"schemaVersion":1,"description":"overwritten"}`)
	if !stale.IsError || stale.Envelope.Errors[0].Code != agentapi.ProfileConflict {
		t.Fatalf("stale set: %+v", stale)
	}
	again, _ := controls.ProfileGet(t.Context(), mcpnative.ProfileGetRequest{Scope: mcpnative.ProfileScopeHost})
	current := again.Envelope.Data.(agentapi.ProfileSection)
	if current.Profile == nil || current.Profile.Description != "build box" || current.Revision == nil || *current.Revision != 1 {
		t.Fatalf("profile after a stale set: %+v", current)
	}
	// An invalid document is refused without writing.
	invalid := set(1, `{"schemaVersion":1,"unknown":true}`)
	if !invalid.IsError || invalid.Envelope.Errors[0].Code != agentapi.ProfileInvalid {
		t.Fatalf("invalid set: %+v", invalid)
	}
	next := set(1, `{"schemaVersion":1,"description":"second"}`)
	if next.IsError || next.Envelope.Data.(agentapi.ProfileWrite).Revision != 2 {
		t.Fatalf("second set: %+v", next)
	}
}

func TestMCPFleetNotesAndStatusReadLocalState(t *testing.T) {
	controls := openProfileTestState(t)
	status, err := controls.FleetStatus(t.Context(), mcpnative.FleetStatusRequest{})
	if err != nil || status.IsError {
		t.Fatalf("fleet status: %+v %v", status, err)
	}
	data := status.Envelope.Data.(agentapi.FleetStatus)
	if data.FleetSaved || len(data.Members) != 0 {
		t.Fatalf("status of a machine without a fleet: %+v", data)
	}
	got, err := controls.ProfileGet(t.Context(), mcpnative.ProfileGetRequest{Scope: mcpnative.ProfileScopeFleet})
	if err != nil || got.IsError {
		t.Fatalf("fleet profile: %+v %v", got, err)
	}
	if _, ok := got.Envelope.Data.(agentapi.FleetSection); !ok {
		t.Fatalf("fleet data: %#v", got.Envelope.Data)
	}
	// Publishing fleet notes works as it does for mesh profile set --fleet, and a
	// stale expected revision conflicts without changing the replica.
	set, err := controls.ProfileSet(t.Context(), mcpnative.ProfileSetRequest{Scope: mcpnative.ProfileScopeFleet, ExpectedRevision: 0, Document: []byte(`{"schemaVersion":1,"description":"fleet"}`)})
	if err != nil || set.IsError || set.Envelope.Data.(agentapi.ProfileWrite).Revision != 1 {
		t.Fatalf("fleet set: %+v %v", set, err)
	}
	stale, err := controls.ProfileSet(t.Context(), mcpnative.ProfileSetRequest{Scope: mcpnative.ProfileScopeFleet, ExpectedRevision: 0, Document: []byte(`{"schemaVersion":1,"description":"stale"}`)})
	if err != nil || !stale.IsError {
		t.Fatalf("stale fleet set: %+v %v", stale, err)
	}
	after, _ := controls.ProfileGet(t.Context(), mcpnative.ProfileGetRequest{Scope: mcpnative.ProfileScopeFleet})
	replica := after.Envelope.Data.(agentapi.FleetSection)
	if replica.Revision == nil || *replica.Revision != 1 || replica.Profile == nil || replica.Profile.Description != "fleet" {
		t.Fatalf("fleet replica after a stale set: %+v", replica)
	}
}

// A daemon that cannot name its account cannot be targeted exactly, so a
// files-capable host without one is refused before any file control is sent.
func TestMCPFileToolsRefuseAHostWithoutAnAccount(t *testing.T) {
	f := newFileHostFixture(t)
	f.account = ""
	f.respond = func(protocol.Control) (protocol.Control, error) {
		return fileResult(t, protocol.TypeFileReadResult, protocol.FileReadResult{}), nil
	}
	out, err := f.controls.FileRead(t.Context(), mcpnative.FileReadRequest{Target: mcpnative.FileTargetRef{Host: filesTestHostID}, Path: "/a", Timeout: 5 * time.Second})
	if err != nil || !out.IsError || out.Envelope.Errors[0].Code != agentapi.ProtocolUnsupported {
		t.Fatalf("a host without an account: %+v %v", out, err)
	}
	if len(f.requests()) != 0 {
		t.Fatalf("a request was sent to a host without an account: %d", len(f.requests()))
	}
}

// Errors after the daemon authorized a context keep the workspace; a failed
// authorization shows the handle that was asked for and no directory.
func TestMCPFileErrorsKeepTheContextAndWorkspace(t *testing.T) {
	f := newFileHostFixture(t)
	f.controls.caller = filesTestCallerID
	handle := mcpnative.ContextRef{HostID: filesTestHostID, ContextID: filesTestContext}
	call := func() mcpnative.CallOutcome {
		out, err := f.controls.FileWrite(t.Context(), mcpnative.FileWriteRequest{Target: mcpnative.FileTargetRef{Context: &handle}, Path: "a.txt", Data: []byte("x"), ExpectedSHA256: strings.Repeat("a", 64), Timeout: 5 * time.Second})
		if err != nil || !out.IsError {
			t.Fatalf("write: %+v %v", out, err)
		}
		return out
	}
	detail, err := protocol.EncodeFileDocument(struct {
		Target protocol.FileTarget `json:"target"`
	}{protocol.FileTarget{HostID: filesTestHostID, Account: "remote-bob", ContextID: filesTestContext, Cwd: "/work/app"}})
	if err != nil {
		t.Fatal(err)
	}
	f.respond = func(protocol.Control) (protocol.Control, error) {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: "file.conflict", Message: "digest differs", Files: detail}, nil
	}
	conflict := call()
	if conflict.Envelope.Errors[0].Code != agentapi.FileConflict || conflict.Target == nil || conflict.Target.ContextID != handle.String() || conflict.Target.Cwd != "/work/app" {
		t.Fatalf("an authorized conflict lost its workspace: %+v", conflict.Target)
	}
	f.respond = func(protocol.Control) (protocol.Control, error) {
		return protocol.Control{Type: protocol.TypeError, ErrorCode: "context.not_found", Message: "no such context"}, nil
	}
	denied := call()
	if denied.Envelope.Errors[0].Code != agentapi.ContextNotFound || denied.Target == nil || denied.Target.ContextID != handle.String() || denied.Target.Cwd != "" {
		t.Fatalf("a refused context must show the handle and no directory: %+v", denied.Target)
	}
	f.respond = func(protocol.Control) (protocol.Control, error) {
		return protocol.Control{}, errors.New("connection reset")
	}
	lost := call()
	if lost.Envelope.Errors[0].Code != agentapi.RequestOutcomeUnknown || lost.Target == nil || lost.Target.Cwd != "" || lost.Target.ContextID != handle.String() {
		t.Fatalf("a lost reply knows no directory: %+v", lost.Target)
	}
}

// Copies are long operations: at most four to one host at a time, and a call
// still queued when it is cancelled never dials.
func TestMCPCopyWaitsForAStreamSlotAndCancelsWithoutDialing(t *testing.T) {
	host := setupCommandTestHost(t)
	var dials atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	dial := func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		dials.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("test dial closed")
	}
	app := &application{dependencies: Dependencies{Now: time.Now, DialControl: dial}}
	pool := newMCPHostPool(func(context.Context, HostRecord) (transport.Conn, error) { return nil, errors.New("unused") }, time.Now)
	defer pool.Close()
	controls := newMCPControls(app, pool, "")
	source := filepath.Join(t.TempDir(), "source.txt")
	writeCopyTestFile(t, source, "payload", 0o600)
	request := mcpnative.CopyRequest{Host: host.host.ID, Direction: mcpnative.CopyUpload, LocalPath: source, RemotePath: "/tmp/remote.txt", Timeout: 30 * time.Second}

	var running sync.WaitGroup
	for range mcpPoolStreamLimit {
		running.Add(1)
		go func() {
			defer running.Done()
			_, _ = controls.Copy(t.Context(), request)
		}()
	}
	for range mcpPoolStreamLimit {
		<-entered
	}
	queued, cancel := context.WithCancel(t.Context())
	finished := make(chan mcpnative.CallOutcome, 1)
	go func() {
		out, _ := controls.Copy(queued, request)
		finished <- out
	}()
	select {
	case <-entered:
		t.Fatalf("a fifth copy dialed with four streams open: %d dials", dials.Load())
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	if out := <-finished; !out.IsError {
		t.Fatalf("a cancelled queued copy succeeded: %+v", out)
	}
	if got := dials.Load(); got != int32(mcpPoolStreamLimit) {
		t.Fatalf("the cancelled copy dialed: %d dials", got)
	}
	close(release)
	running.Wait()
	// Every slot was released, so a later copy gets one.
	if _, err := controls.Copy(t.Context(), request); err != nil || dials.Load() != int32(mcpPoolStreamLimit)+1 {
		t.Fatalf("a slot was not released: %v, %d dials", err, dials.Load())
	}
}
