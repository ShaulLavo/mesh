package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

const fileTestHost = "host-files"

type fileFixture struct {
	t          *testing.T
	controller *fileController
	root       string
}

// resolvedPath removes symlinks from a scratch path, because the file controls
// refuse them anywhere (macOS keeps temporary directories behind /var and /tmp).
func resolvedPath(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func newFileFixture(t *testing.T) *fileFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fileFixture{t: t, controller: newFileController(fileTestHost), root: root}
}

func (f *fileFixture) target() protocol.FileTarget {
	return protocol.FileTarget{HostID: fileTestHost, Account: f.controller.account}
}

func (f *fileFixture) path(name string) string { return filepath.Join(f.root, name) }

func (f *fileFixture) put(name string, data []byte) string {
	f.t.Helper()
	path := f.path(name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fileFixture) call(requestType string, body any) protocol.Control {
	f.t.Helper()
	return callFiles(f.t, f.controller, requestType, body)
}

func callFiles(t *testing.T, controller *fileController, requestType string, body any) protocol.Control {
	t.Helper()
	raw, err := protocol.EncodeFileDocument(body)
	if err != nil {
		t.Fatal(err)
	}
	response, handled, err := controller.HandleControl(localServerContext(t.Context()), protocol.Control{Type: requestType, RequestID: "files", Files: raw})
	if err != nil || !handled {
		t.Fatalf("%s: handled=%v err=%v", requestType, handled, err)
	}
	return response
}

func fileOK[T any](t *testing.T, response protocol.Control, want string) T {
	t.Helper()
	if response.Type != want {
		t.Fatalf("response %s (%s: %s), want %s", response.Type, response.ErrorCode, response.Message, want)
	}
	var result T
	if err := protocol.DecodeFileDocument(response.Files, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func fileCode(t *testing.T, response protocol.Control, want string) {
	t.Helper()
	if response.Type != protocol.TypeError || response.ErrorCode != want {
		t.Fatalf("response %s code %q (%s), want error %s", response.Type, response.ErrorCode, response.Message, want)
	}
}

func (f *fileFixture) read(request protocol.FileReadRequest) protocol.Control {
	request.Target = f.target()
	return f.call(protocol.TypeFileRead, request)
}

func (f *fileFixture) readPage(request protocol.FileReadRequest) protocol.FileReadResult {
	f.t.Helper()
	return fileOK[protocol.FileReadResult](f.t, f.read(request), protocol.TypeFileReadResult)
}

func pageBytes(t *testing.T, page protocol.FileReadResult) []byte {
	t.Helper()
	if page.Encoding == protocol.FileEncodingUTF8 {
		return []byte(page.Data)
	}
	data, err := base64.StdEncoding.DecodeString(page.Data)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f *fileFixture) readAll(path string, maxBytes int) []byte {
	f.t.Helper()
	var assembled []byte
	request := protocol.FileReadRequest{Path: path, MaxBytes: maxBytes}
	for {
		page := f.readPage(request)
		if page.Start != uint64(len(assembled)) {
			f.t.Fatalf("page starts at %d after %d bytes", page.Start, len(assembled))
		}
		data := pageBytes(f.t, page)
		if uint64(len(data)) != page.End-page.Start {
			f.t.Fatalf("page carries %d bytes for range %d-%d", len(data), page.Start, page.End)
		}
		assembled = append(assembled, data...)
		if page.EOF {
			return assembled
		}
		request = protocol.FileReadRequest{Path: path, Cursor: page.NextCursor, MaxBytes: maxBytes}
	}
}

// backdate moves a file's modification time into the past, so a later in-place
// write always changes it even on a filesystem with coarse timestamps.
func (f *fileFixture) backdate(path string) {
	f.t.Helper()
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fileFixture) write(request protocol.FileWriteRequest) protocol.Control {
	request.Target = f.target()
	return f.call(protocol.TypeFileWrite, request)
}

func (f *fileFixture) patch(request protocol.FilePatchRequest) protocol.Control {
	request.Target = f.target()
	return f.call(protocol.TypeFilePatch, request)
}

func (f *fileFixture) requireContent(path string, want []byte) {
	f.t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // the test names its own temporary file
	if err != nil || !bytes.Equal(got, want) {
		f.t.Fatalf("%s holds %d bytes (err %v), want %d unchanged", path, len(got), err, len(want))
	}
}

func (f *fileFixture) requireNoTemporaryFiles() {
	f.t.Helper()
	entries, err := os.ReadDir(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mesh-fs-") {
			f.t.Fatalf("temporary file %s survived", entry.Name())
		}
	}
}

func patternBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte('a' + i%23)
		if i%64 == 63 {
			data[i] = '\n'
		}
	}
	return data
}

func TestFileTargetRefusesOtherHostAndAccount(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("a.txt", []byte("alpha\n"))
	for name, target := range map[string]protocol.FileTarget{
		"other host":    {HostID: "someone-else", Account: f.controller.account},
		"no host":       {Account: f.controller.account},
		"other account": {HostID: fileTestHost, Account: f.controller.account + "-other"},
	} {
		t.Run(name, func(t *testing.T) {
			want := "file.target_mismatch"
			if target.HostID != fileTestHost {
				want = "host.identity_mismatch"
			}
			fileCode(t, f.call(protocol.TypeFileRead, protocol.FileReadRequest{Target: target, Path: path}), want)
			fileCode(t, f.call(protocol.TypeFileWrite, protocol.FileWriteRequest{Target: target, Path: f.path("new.txt"), Data: "x", CreateOnly: true}), want)
			fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: target, Path: f.root}), want)
			fileCode(t, f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: target, Path: f.root, Pattern: "a"}), want)
			fileCode(t, f.call(protocol.TypeFilePatch, protocol.FilePatchRequest{Target: target, Path: path, ExpectedSHA256: digestHex([]byte("alpha\n")), Edits: []protocol.FileEdit{{OldText: "alpha", NewText: "beta"}}}), want)
		})
	}
	if _, err := os.Stat(f.path("new.txt")); err == nil {
		t.Fatal("a refused write created a file")
	}
	f.requireContent(path, []byte("alpha\n"))
	// An omitted account is allowed, and every result names the real one.
	page := fileOK[protocol.FileReadResult](t, f.call(protocol.TypeFileRead, protocol.FileReadRequest{Target: protocol.FileTarget{HostID: fileTestHost}, Path: path}), protocol.TypeFileReadResult)
	if page.Target != f.target() {
		t.Fatalf("result target %+v, want %+v", page.Target, f.target())
	}
}

func TestFileRequestsNeedAbsolutePathsAndStrictDocuments(t *testing.T) {
	f := newFileFixture(t)
	f.put("a.txt", []byte("alpha"))
	for _, path := range []string{"", "a.txt", "./a.txt", "../a.txt", f.root + "/a\x00.txt", "/" + strings.Repeat("x", protocol.MaxFilePath)} {
		fileCode(t, f.read(protocol.FileReadRequest{Path: path}), "usage.invalid")
	}
	for _, document := range []string{``, `{"target":{"hostId":"` + fileTestHost + `"},"path":"/x","unknown":1}`, `{} {}`, `[]`} {
		response, _, _ := f.controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeFileRead, RequestID: "r", Files: json.RawMessage(document)})
		fileCode(t, response, "usage.invalid")
	}
	if _, _, err := f.controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeFileRead, Files: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("request without an ID was accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := f.controller.HandleControl(ctx, protocol.Control{Type: protocol.TypeFileRead, RequestID: "r", Files: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("cancelled request ran")
	}
	if _, handled, _ := f.controller.HandleControl(t.Context(), protocol.Control{Type: protocol.TypeHostInfo, RequestID: "r"}); handled {
		t.Fatal("file controller claimed another control")
	}
}

func TestFileReadPagesCoverTheFileAndCarryItsDigest(t *testing.T) {
	f := newFileFixture(t)
	want := patternBytes(2*protocol.MaxFilePage + 12345)
	path := f.put("big.txt", want)
	first := f.readPage(protocol.FileReadRequest{Path: path})
	if first.Encoding != protocol.FileEncodingUTF8 || first.End-first.Start != protocol.MaxFilePage || first.EOF || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	if first.Size != uint64(len(want)) || first.SHA256 != digestHex(want) || first.Revision == "" {
		t.Fatalf("first page metadata: size %d digest %s", first.Size, first.SHA256)
	}
	if got := f.readAll(path, 0); !bytes.Equal(got, want) {
		t.Fatalf("assembled %d bytes, want %d", len(got), len(want))
	}
	small := f.readPage(protocol.FileReadRequest{Path: path, MaxBytes: 100})
	if small.End != 100 || len(small.Data) != 100 {
		t.Fatalf("small page: %+v", small.End)
	}
	tail := f.readPage(protocol.FileReadRequest{Path: path, Offset: uint64(len(want) - 5)}) //nolint:gosec // the test file is longer than five bytes
	if !tail.EOF || tail.NextCursor != "" || tail.End != uint64(len(want)) {
		t.Fatalf("tail page: %+v", tail)
	}
	past := f.readPage(protocol.FileReadRequest{Path: path, Offset: uint64(len(want)) + 10})
	if !past.EOF || past.Data != "" || past.Start != uint64(len(want))+10 {
		t.Fatalf("page past the end: %+v", past)
	}
}

func TestFileReadEncodesBinaryAndOversizedText(t *testing.T) {
	f := newFileFixture(t)
	cases := map[string][]byte{
		"nul":     []byte("text\x00more"),
		"invalid": {0xff, 0xfe, 'a', 0x80},
		// One mebibyte of control characters escapes to six times its size.
		"escapes": bytes.Repeat([]byte{0x01}, protocol.MaxFilePage),
		// HTML-sensitive characters are escaped by the wire encoder too.
		"angle": bytes.Repeat([]byte("<"), protocol.MaxFilePage),
	}
	for name, data := range cases {
		path := f.put(name, data)
		page := f.readPage(protocol.FileReadRequest{Path: path})
		if page.Encoding != protocol.FileEncodingBase64 || !bytes.Equal(pageBytes(t, page), data) {
			t.Fatalf("%s: encoding %s", name, page.Encoding)
		}
		frames, err := protocol.ControlFrames(f.call(protocol.TypeFileRead, protocol.FileReadRequest{Target: f.target(), Path: path}))
		if err != nil || len(frames) != 1 || len(frames[0].Payload) >= protocol.MaxPayload {
			t.Fatalf("%s: reply does not fit one frame: %v", name, err)
		}
	}
	text := f.put("text.txt", []byte("héllo wörld ☃\n"))
	if page := f.readPage(protocol.FileReadRequest{Path: text}); page.Encoding != protocol.FileEncodingUTF8 || page.Data != "héllo wörld ☃\n" {
		t.Fatalf("text page: %+v", page)
	}
}

func TestFilePagesNeverSplitACharacter(t *testing.T) {
	f := newFileFixture(t)
	want := []byte(strings.Repeat("é☃😀", 40000))
	path := f.put("unicode.txt", want)
	// 1003 bytes end two bytes into a three byte character, so the page gives
	// that fragment up and ends at 1001.
	page := f.readPage(protocol.FileReadRequest{Path: path, MaxBytes: 1003})
	if page.Encoding != protocol.FileEncodingUTF8 || !utf8Valid(page.Data) || page.End != 1001 {
		t.Fatalf("page split a character: encoding %s end %d", page.Encoding, page.End)
	}
	if got := f.readAll(path, 1003); !bytes.Equal(got, want) {
		t.Fatalf("assembled %d bytes, want %d", len(got), len(want))
	}
	mid := f.readPage(protocol.FileReadRequest{Path: path, Offset: 1, MaxBytes: 10})
	if mid.Encoding != protocol.FileEncodingBase64 || !bytes.Equal(pageBytes(t, mid), want[1:11]) {
		t.Fatalf("a page starting inside a character must be exact bytes: %+v", mid)
	}
}

func utf8Valid(text string) bool { return !strings.ContainsRune(text, '�') }

func TestFileReadBounds(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("a.txt", patternBytes(4096))
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, MaxBytes: protocol.MaxFilePage + 1}), "usage.invalid")
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, MaxBytes: -1}), "usage.invalid")
	fileCode(t, f.read(protocol.FileReadRequest{Path: f.path("missing")}), "file.not_found")
	page := f.readPage(protocol.FileReadRequest{Path: path, MaxBytes: protocol.MaxFilePage})
	if page.End != 4096 || !page.EOF {
		t.Fatalf("page: %+v", page)
	}
	large := f.path("large.bin")
	file, err := os.Create(large) //nolint:gosec // the test names its own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(protocol.MaxFileDigestSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if page := f.readPage(protocol.FileReadRequest{Path: large, MaxBytes: 10}); page.SHA256 != "" {
		t.Fatal("a large file was hashed without being asked")
	}
	if page := f.readPage(protocol.FileReadRequest{Path: large, MaxBytes: 10, Digest: true}); len(page.SHA256) != 64 {
		t.Fatalf("requested digest missing: %q", page.SHA256)
	}
}

func TestFileReadContinuationFailsAfterReplacementOrChange(t *testing.T) {
	f := newFileFixture(t)
	original := patternBytes(2*protocol.MaxFilePage + 100)
	path := f.put("log.txt", original)
	f.backdate(path)
	first := f.readPage(protocol.FileReadRequest{Path: path})
	next := protocol.FileReadRequest{Path: path, Cursor: first.NextCursor}
	if second := f.readPage(next); second.Start != first.End {
		t.Fatalf("unchanged continuation starts at %d", second.Start)
	}

	// Replacement: a different file of identical size and content renamed over it.
	replacement := f.put("replacement.txt", original)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	fileCode(t, f.read(next), "file.changed")

	// A fresh read works, and an in-place rewrite of the same size also invalidates.
	f.backdate(path)
	again := f.readPage(protocol.FileReadRequest{Path: path})
	next = protocol.FileReadRequest{Path: path, Cursor: again.NextCursor}
	rewrite, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // the test names its own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rewrite.WriteAt([]byte("Z"), 7); err != nil {
		t.Fatal(err)
	}
	_ = rewrite.Close()
	fileCode(t, f.read(next), "file.changed")

	// Growth and removal invalidate it too.
	again = f.readPage(protocol.FileReadRequest{Path: path})
	next = protocol.FileReadRequest{Path: path, Cursor: again.NextCursor}
	appendTo, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // the test names its own temporary file
	if err != nil {
		t.Fatal(err)
	}
	_, _ = appendTo.WriteString("more\n")
	_ = appendTo.Close()
	fileCode(t, f.read(next), "file.changed")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fileCode(t, f.read(next), "file.not_found")
}

func TestFileReadCursorIsBoundToHostAccountPathAndOperation(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("a.txt", patternBytes(3*protocol.MaxFilePage))
	other := f.put("b.txt", patternBytes(3*protocol.MaxFilePage))
	page := f.readPage(protocol.FileReadRequest{Path: path})
	fileCode(t, f.read(protocol.FileReadRequest{Path: other, Cursor: page.NextCursor}), "file.cursor_invalid")
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: "not a cursor"}), "file.cursor_invalid")
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: strings.Repeat("A", maxFileCursor+1)}), "file.cursor_invalid")
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: page.NextCursor, Offset: 5}), "usage.invalid")

	// A cursor minted for another host or account is refused even for the same path.
	otherHost := &fileController{hostID: "another-host", account: f.controller.account}
	foreign := otherHost.encodeCursor(fileCursor{Kind: fileCursorRead, Offset: 5, Revision: page.Revision}, path)
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: foreign}), "file.cursor_invalid")
	otherAccount := &fileController{hostID: fileTestHost, account: f.controller.account + "-other"}
	foreign = otherAccount.encodeCursor(fileCursor{Kind: fileCursorRead, Offset: 5, Revision: page.Revision}, path)
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: foreign}), "file.cursor_invalid")
	listCursor := f.controller.encodeCursor(fileCursor{Kind: fileCursorList, After: "x"}, path)
	fileCode(t, f.read(protocol.FileReadRequest{Path: path, Cursor: listCursor}), "file.cursor_invalid")
}

func TestFileWriteReplacesWithDigestAndKeepsMode(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("config.txt", []byte("old\n"))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	result := fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: path, Data: "new\n", ExpectedSHA256: strings.ToUpper(digestHex([]byte("old\n")))}), protocol.TypeFileWriteResult)
	if result.Created || result.Size != 4 || result.SHA256 != digestHex([]byte("new\n")) || result.Revision == "" {
		t.Fatalf("result: %+v", result)
	}
	f.requireContent(path, []byte("new\n"))
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	f.requireNoTemporaryFiles()
}

func TestFileWriteDigestConflictDoesNotMutate(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("config.txt", []byte("old\n"))
	before, _ := os.Stat(path)
	for _, digest := range []string{digestHex([]byte("something else")), strings.Repeat("0", 64)} {
		fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "new\n", ExpectedSHA256: digest}), "file.conflict")
	}
	f.requireContent(path, []byte("old\n"))
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a conflicting write touched the file")
	}
	fileCode(t, f.write(protocol.FileWriteRequest{Path: f.path("missing"), Data: "x", ExpectedSHA256: strings.Repeat("0", 64)}), "file.not_found")
	for _, digest := range []string{"abc", strings.Repeat("g", 64)} {
		fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "x", ExpectedSHA256: digest}), "usage.invalid")
	}
	f.requireNoTemporaryFiles()
}

func TestFileWriteNeedsExactlyOnePrecondition(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("a.txt", []byte("a"))
	fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "x"}), "usage.invalid")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "x", CreateOnly: true, ExpectedSHA256: digestHex([]byte("a"))}), "usage.invalid")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "x", CreateOnly: true, Encoding: "latin-1"}), "usage.invalid")
	f.requireContent(path, []byte("a"))
}

func TestFileWriteCreateOnly(t *testing.T) {
	f := newFileFixture(t)
	path := f.path("created.txt")
	result := fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: path, Data: "first", CreateOnly: true}), protocol.TypeFileWriteResult)
	if !result.Created || result.SHA256 != digestHex([]byte("first")) {
		t.Fatalf("result: %+v", result)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != os.FileMode(newFilePermission) {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "second", CreateOnly: true}), "file.conflict")
	f.requireContent(path, []byte("first"))
	fileCode(t, f.write(protocol.FileWriteRequest{Path: f.path("no-such-dir/x"), Data: "x", CreateOnly: true}), "file.not_found")
	f.requireNoTemporaryFiles()
}

func TestFileWriteEncodingAndBounds(t *testing.T) {
	f := newFileFixture(t)
	binary := []byte{0, 1, 2, 0xff, 0xfe, 'x'}
	path := f.path("blob")
	fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: path, Encoding: protocol.FileEncodingBase64, Data: base64.StdEncoding.EncodeToString(binary), CreateOnly: true}), protocol.TypeFileWriteResult)
	f.requireContent(path, binary)

	full := bytes.Repeat([]byte("x"), protocol.MaxFileWrite)
	exact := f.path("exact")
	fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: exact, Data: string(full), CreateOnly: true}), protocol.TypeFileWriteResult)
	f.requireContent(exact, full)

	tooLarge := f.path("too-large")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: tooLarge, Data: string(full) + "x", CreateOnly: true}), "resource.limit")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: tooLarge, Encoding: protocol.FileEncodingBase64, Data: base64.StdEncoding.EncodeToString(append(full, 'x')), CreateOnly: true}), "resource.limit")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: tooLarge, Encoding: protocol.FileEncodingBase64, Data: "!!!", CreateOnly: true}), "usage.invalid")
	if _, err := os.Stat(tooLarge); err == nil {
		t.Fatal("an oversized write created a file")
	}
	f.requireNoTemporaryFiles()
}

func TestFileWritesSerializeOnTheirDigest(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("counter.txt", []byte("start"))
	digest := digestHex([]byte("start"))
	const writers = 8
	var wins, conflicts int
	var mutex sync.Mutex
	var group sync.WaitGroup
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			response := f.write(protocol.FileWriteRequest{Path: path, Data: fmt.Sprintf("writer %d", i), ExpectedSHA256: digest})
			mutex.Lock()
			defer mutex.Unlock()
			switch {
			case response.Type == protocol.TypeFileWriteResult:
				wins++
			case response.ErrorCode == "file.conflict":
				conflicts++
			default:
				t.Errorf("unexpected response %s %s", response.Type, response.ErrorCode)
			}
		}()
	}
	group.Wait()
	if wins != 1 || conflicts != writers-1 {
		t.Fatalf("wins %d conflicts %d", wins, conflicts)
	}
	f.requireNoTemporaryFiles()
}

func TestFilePatchAppliesExactEdits(t *testing.T) {
	f := newFileFixture(t)
	content := []byte("one\ntwo\nthree\n")
	path := f.put("p.txt", content)
	if err := os.Chmod(path, 0o750); err != nil { //nolint:gosec // a mode other than the default shows that patching keeps it
		t.Fatal(err)
	}
	result := fileOK[protocol.FilePatchResult](t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digestHex(content), Edits: []protocol.FileEdit{{OldText: "two", NewText: "2"}, {OldText: "2\nthree", NewText: "2 and 3"}}}), protocol.TypeFilePatchResult)
	want := []byte("one\n2 and 3\n")
	if result.Edits != 2 || result.Size != uint64(len(want)) || result.SHA256 != digestHex(want) {
		t.Fatalf("result: %+v", result)
	}
	f.requireContent(path, want)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o750 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	f.requireNoTemporaryFiles()
}

func TestFilePatchFailuresDoNotMutate(t *testing.T) {
	f := newFileFixture(t)
	content := []byte("same\nsame\nother\n")
	path := f.put("p.txt", content)
	digest := digestHex(content)
	edit := func(oldText, newText string) []protocol.FileEdit {
		return []protocol.FileEdit{{OldText: oldText, NewText: newText}}
	}
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: strings.Repeat("0", 64), Edits: edit("other", "x")}), "file.conflict")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: edit("same", "x")}), "file.patch_mismatch")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: edit("absent", "x")}), "file.patch_mismatch")
	// The first edit is fine and the second is not: nothing may land.
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: append(edit("other", "changed"), protocol.FileEdit{OldText: "absent", NewText: "x"})}), "file.patch_mismatch")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: edit("", "x")}), "usage.invalid")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest}), "usage.invalid")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: "short", Edits: edit("other", "x")}), "usage.invalid")
	tooMany := make([]protocol.FileEdit, protocol.MaxFilePatchEdits+1)
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: tooMany}), "usage.invalid")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: f.path("missing"), ExpectedSHA256: digest, Edits: edit("a", "b")}), "file.not_found")
	f.requireContent(path, content)
	f.requireNoTemporaryFiles()

	huge := f.path("huge.txt")
	file, err := os.Create(huge) //nolint:gosec // the test names its own temporary file
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(protocol.MaxFilePatchSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: huge, ExpectedSHA256: digest, Edits: edit("a", "b")}), "resource.limit")
}

func TestFileOperationsRefuseSymlinksAndSpecialFiles(t *testing.T) {
	f := newFileFixture(t)
	regular := f.put("real.txt", []byte("real content\n"))
	link := f.path("link.txt")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	dangling := f.path("dangling.txt")
	if err := os.Symlink(f.path("nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	fifo := f.path("pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(resolvedPath(t, compactSocketTempDir(t)), "s")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // Test cleanup.
	digest := digestHex([]byte("real content\n"))
	for _, path := range []string{link, dangling, fifo, socketPath, f.root} {
		fileCode(t, f.read(protocol.FileReadRequest{Path: path}), "file.not_regular")
		fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "x", ExpectedSHA256: digest}), "file.not_regular")
		fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: []protocol.FileEdit{{OldText: "real", NewText: "x"}}}), "file.not_regular")
	}
	f.requireContent(regular, []byte("real content\n"))
	// A create-only write onto an existing name of any kind loses to it.
	for _, path := range []string{link, dangling, fifo} {
		fileCode(t, f.write(protocol.FileWriteRequest{Path: path, Data: "x", CreateOnly: true}), "file.conflict")
	}
	fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: link}), "file.not_regular")
	fileCode(t, f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: link, Pattern: "real"}), "file.not_regular")
	fileCode(t, f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: fifo, Pattern: "real"}), "file.not_regular")
	if target, _ := os.Readlink(link); target != regular {
		t.Fatal("the symlink was replaced")
	}
	f.requireNoTemporaryFiles()
}

func TestFileListPagesInNameOrderWithoutFollowingLinks(t *testing.T) {
	f := newFileFixture(t)
	for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
		f.put(name, []byte(name))
	}
	if err := os.Mkdir(f.path("dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.path("a.txt"), f.path("link")); err != nil {
		t.Fatal(err)
	}
	request := protocol.FileListRequest{Target: f.target(), Path: f.root, Limit: 2}
	var names, kinds []string
	for {
		page := fileOK[protocol.FileListResult](t, f.call(protocol.TypeFileList, request), protocol.TypeFileListResult)
		for _, entry := range page.Entries {
			names, kinds = append(names, entry.Name), append(kinds, entry.Kind)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Entries) != 2 {
			t.Fatalf("short page: %d entries", len(page.Entries))
		}
		request.Cursor = page.NextCursor
	}
	if got, want := strings.Join(names, ","), "a.txt,b.txt,c.txt,dir,link"; got != want {
		t.Fatalf("names %s, want %s", got, want)
	}
	if got, want := strings.Join(kinds, ","), "file,file,file,dir,symlink"; got != want {
		t.Fatalf("kinds %s, want %s", got, want)
	}
	foreign := f.controller.encodeCursor(fileCursor{Kind: fileCursorList, After: "a"}, f.path("dir"))
	fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.root, Cursor: foreign}), "file.cursor_invalid")
	fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.root, Limit: protocol.MaxFileList + 1}), "usage.invalid")
	fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.path("a.txt")}), "file.not_regular")
	fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.path("missing")}), "file.not_found")
}

func (f *fileFixture) search(request protocol.FileSearchRequest) protocol.FileSearchResult {
	f.t.Helper()
	request.Target = f.target()
	return fileOK[protocol.FileSearchResult](f.t, f.call(protocol.TypeFileSearch, request), protocol.TypeFileSearchResult)
}

func TestFileSearchMatchesTextFilesWithinBounds(t *testing.T) {
	f := newFileFixture(t)
	f.put("a.go", []byte("package a\nfunc Alpha() {}\nfunc Beta() {}\n"))
	f.put("b.txt", []byte("Alpha in text\r\nno match\n"))
	f.put("binary.bin", append([]byte("Alpha\x00"), 'x'))
	big := make([]byte, protocol.MaxFileSearchSize+1)
	copy(big, "Alpha")
	f.put("big.txt", big)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("Alpha outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.path("escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.path("a.go"), f.path("alias.go")); err != nil {
		t.Fatal(err)
	}
	result := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: `Al\w+`})
	var got []string
	for _, match := range result.Matches {
		got = append(got, fmt.Sprintf("%s:%d:%d:%s", filepath.Base(match.Path), match.Line, match.Column, match.Text))
	}
	if want := "a.go:2:6:func Alpha() {},b.txt:1:1:Alpha in text"; strings.Join(got, ",") != want {
		t.Fatalf("matches %v, want %s", got, want)
	}
	if result.Truncated != "" || result.Skipped != 4 || result.FilesVisited != 6 {
		t.Fatalf("visited %d skipped %d truncated %q", result.FilesVisited, result.Skipped, result.Truncated)
	}
	only := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "Alpha", Glob: "*.go"})
	if len(only.Matches) != 1 || filepath.Base(only.Matches[0].Path) != "a.go" {
		t.Fatalf("glob matches: %+v", only.Matches)
	}
	file := f.search(protocol.FileSearchRequest{Path: f.path("b.txt"), Pattern: "match"})
	if len(file.Matches) != 1 || file.Matches[0].Line != 2 {
		t.Fatalf("single file search: %+v", file.Matches)
	}
	none := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "zzzz"})
	if none.Matches == nil || len(none.Matches) != 0 {
		t.Fatal("an empty search must report an empty list")
	}
}

func TestFileSearchRejectsBadPatterns(t *testing.T) {
	f := newFileFixture(t)
	f.put("a.txt", []byte("a"))
	for _, request := range []protocol.FileSearchRequest{
		{Pattern: ""}, {Pattern: "("}, {Pattern: `(a)\1`}, {Pattern: strings.Repeat("a", protocol.MaxFilePattern+1)},
		{Pattern: "a", Glob: "[x"}, {Pattern: "a", MaxMatches: -1}, {Pattern: "a", MaxMatches: protocol.MaxFileSearchMatches + 1},
	} {
		request.Target, request.Path = f.target(), f.root
		fileCode(t, f.call(protocol.TypeFileSearch, request), "usage.invalid")
	}
	fileCode(t, f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: f.path("missing"), Pattern: "a"}), "file.not_found")
}

func TestFileSearchStopsAtMatchAndFileLimits(t *testing.T) {
	f := newFileFixture(t)
	f.put("many.txt", []byte(strings.Repeat("hit\n", 50)))
	exact := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "hit", MaxMatches: 50})
	if len(exact.Matches) != 50 || exact.Truncated != "" {
		t.Fatalf("a search that found exactly the limit is complete: %d %q", len(exact.Matches), exact.Truncated)
	}
	limited := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "hit", MaxMatches: 10})
	if len(limited.Matches) != 10 || limited.Truncated != protocol.FileTruncatedMatches {
		t.Fatalf("limited: %d %q", len(limited.Matches), limited.Truncated)
	}

	crowd := resolvedPath(t, t.TempDir())
	for i := range protocol.MaxFileSearchFiles + 5 {
		if err := os.WriteFile(filepath.Join(crowd, fmt.Sprintf("f%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	visited := f.search(protocol.FileSearchRequest{Path: crowd, Pattern: "x"})
	if visited.FilesVisited != protocol.MaxFileSearchFiles || visited.Truncated != protocol.FileTruncatedFiles {
		t.Fatalf("visited %d truncated %q", visited.FilesVisited, visited.Truncated)
	}
}

func TestFileSearchBoundsTheReply(t *testing.T) {
	f := newFileFixture(t)
	line := strings.Repeat("x", 3*protocol.MaxFileMatchText) + "\n"
	f.put("long.txt", []byte(strings.Repeat(line, 1200)))
	result := f.search(protocol.FileSearchRequest{Path: f.root, Pattern: "x"})
	if result.Truncated != protocol.FileTruncatedReply || len(result.Matches) == 0 {
		t.Fatalf("matches %d truncated %q", len(result.Matches), result.Truncated)
	}
	for _, match := range result.Matches {
		if len(match.Text) > protocol.MaxFileMatchText {
			t.Fatalf("match text of %d bytes", len(match.Text))
		}
	}
	frames, err := protocol.ControlFrames(f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: f.root, Pattern: "x"}))
	if err != nil || len(frames) != 1 {
		t.Fatalf("reply does not fit one frame: %v", err)
	}
}

func TestFileControlsAreServedThroughTheClientServer(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("a.txt", []byte("served\n"))
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, &contextWorkerCounter{})
	server, err := newClientServer(lifecycle, &contextWorkerCounter{}, &contextControlCounter{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.files = f.controller
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(localServerContext(t.Context()), client) }()
	raw, err := protocol.EncodeFileDocument(protocol.FileReadRequest{Target: f.target(), Path: path})
	if err != nil {
		t.Fatal(err)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeFileRead, RequestID: "served", Files: raw}))
	response := decodeServerControl(t, client.nextWrite(t))
	if page := fileOK[protocol.FileReadResult](t, response, protocol.TypeFileReadResult); page.Data != "served\n" || response.RequestID != "served" {
		t.Fatalf("response: %+v", page)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeFileRead, RequestID: "wrong-host", Files: json.RawMessage(`{"target":{"hostId":"nobody"},"path":"/x"}`)}))
	fileCode(t, decodeServerControl(t, client.nextWrite(t)), "host.identity_mismatch")
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "file server"); err != nil {
		t.Fatal(err)
	}
}

func TestHostInfoAdvertisesFileControls(t *testing.T) {
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, &contextWorkerCounter{})
	if !lifecycle.declaredHostInfo().FilesSupported {
		t.Fatal("host.info does not advertise file controls")
	}
}

func TestFileDeadlineStopsLongSearch(t *testing.T) {
	f := newFileFixture(t)
	f.put("a.txt", []byte("a"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, err := f.controller.search(ctx, protocol.FileSearchRequest{Target: f.target(), Path: f.root, Pattern: "a"}); err == nil {
		t.Fatal("search ignored its context")
	}
}
