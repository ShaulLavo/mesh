package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

// A symlink anywhere in a path is refused, not only in its last element, so a
// parent that is swapped for a link cannot send an operation elsewhere.
func TestFileOperationsRefuseSymlinkedParents(t *testing.T) {
	f := newFileFixture(t)
	if err := os.Mkdir(f.path("real"), 0o750); err != nil {
		t.Fatal(err)
	}
	inside := f.put("real/a.txt", []byte("alpha\n"))
	if err := os.Symlink(f.path("real"), f.path("link")); err != nil {
		t.Fatal(err)
	}
	viaLink := f.path("link/a.txt")
	digest := digestHex([]byte("alpha\n"))
	fileCode(t, f.read(protocol.FileReadRequest{Path: viaLink}), "file.not_regular")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: viaLink, Data: "x", ExpectedSHA256: digest}), "file.not_regular")
	fileCode(t, f.write(protocol.FileWriteRequest{Path: f.path("link/new.txt"), Data: "x", CreateOnly: true}), "file.not_regular")
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: viaLink, ExpectedSHA256: digest, Edits: []protocol.FileEdit{{OldText: "alpha", NewText: "x"}}}), "file.not_regular")
	fileCode(t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.path("link")}), "file.not_regular")
	fileCode(t, f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: f.path("link"), Pattern: "alpha"}), "file.not_regular")
	if _, err := os.Stat(f.path("real/new.txt")); err == nil {
		t.Fatal("a write through a symlinked parent created a file")
	}
	f.requireContent(inside, []byte("alpha\n"))
	response := f.read(protocol.FileReadRequest{Path: viaLink})
	if !strings.Contains(response.Message, f.path("real")) {
		t.Fatalf("the refusal does not name the link's target: %s", response.Message)
	}
}

// Directories count against the traversal budget as much as files do, so a
// tree of empty directories cannot run past the advertised bound.
func TestFileSearchCountsDirectoriesAgainstTraversalBudget(t *testing.T) {
	f := newFileFixture(t)
	crowd := f.path("crowd")
	if err := os.Mkdir(crowd, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range protocol.MaxFileSearchFiles + 50 {
		if err := os.Mkdir(filepath.Join(crowd, fmt.Sprintf("d%05d", i)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	result := f.search(protocol.FileSearchRequest{Path: crowd, Pattern: "x"})
	if result.Truncated != protocol.FileTruncatedFiles || result.DirectoriesVisited > protocol.MaxFileSearchFiles {
		t.Fatalf("directories visited %d, truncated %q", result.DirectoriesVisited, result.Truncated)
	}
}

// The reply budget counts the serialized match: "<" is six bytes on the wire.
func TestFileSearchBudgetsEncodedMatches(t *testing.T) {
	f := newFileFixture(t)
	markup := strings.Repeat("<", protocol.MaxFileMatchText)
	f.put("markup.txt", []byte(strings.Repeat(markup+"\n", 1000)))
	deep := f.root
	for range 6 {
		deep = filepath.Join(deep, strings.Repeat("<", 200))
		if err := os.Mkdir(deep, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(deep, "more.txt"), []byte(strings.Repeat(markup+"\n", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	response := f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: f.root, Pattern: "<"})
	result := fileOK[protocol.FileSearchResult](t, response, protocol.TypeFileSearchResult)
	if result.Truncated != protocol.FileTruncatedReply {
		t.Fatalf("truncated %q with %d matches", result.Truncated, len(result.Matches))
	}
	frames, err := protocol.ControlFrames(response)
	if err != nil || len(frames) != 1 {
		t.Fatalf("an escape-heavy search must still fit one frame: %v", err)
	}
	if size := len(response.Files); size > 2*protocol.MaxFileSearchReply {
		t.Fatalf("encoded matches take %d bytes, the budget is %d", size, protocol.MaxFileSearchReply)
	}
}

// A cursor is a fixed size however long or escape-heavy the path, and a read
// cursor always names the revision it was issued for.
func TestFileReadCursorSurvivesALongEscapeHeavyPath(t *testing.T) {
	f := newFileFixture(t)
	dir := f.root
	for len(dir)+256 < protocol.MaxFilePath-16 {
		dir = filepath.Join(dir, strings.Repeat("<", 250))
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "data.txt")
	want := patternBytes(protocol.MaxFilePage + 4096)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(path) < protocol.MaxFilePath-300 {
		t.Fatalf("test path is only %d bytes", len(path))
	}
	first := f.readPage(protocol.FileReadRequest{Path: path})
	if first.NextCursor == "" || len(first.NextCursor) > maxFileCursor {
		t.Fatalf("cursor of %d bytes", len(first.NextCursor))
	}
	if got := f.readAll(path, 0); string(got) != string(want) {
		t.Fatalf("assembled %d bytes, want %d", len(got), len(want))
	}
}

// Two occurrences of "aa" in "aaa" overlap, and either could be the one meant.
func TestFilePatchRejectsOverlappingMatches(t *testing.T) {
	f := newFileFixture(t)
	path := f.put("p.txt", []byte("aaa"))
	fileCode(t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digestHex([]byte("aaa")), Edits: []protocol.FileEdit{{OldText: "aa", NewText: "b"}}}), "file.patch_mismatch")
	f.requireContent(path, []byte("aaa"))
	fileOK[protocol.FilePatchResult](t, f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digestHex([]byte("aaa")), Edits: []protocol.FileEdit{{OldText: "aaa", NewText: "b"}}}), protocol.TypeFilePatchResult)
	f.requireContent(path, []byte("b"))
}
