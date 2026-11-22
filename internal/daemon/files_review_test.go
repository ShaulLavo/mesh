package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

// A symlinked parent directory resolves once, up front, and the result and its
// cursors name the canonical path. The last element is never resolved.
func TestFileOperationsResolveSymlinkedParents(t *testing.T) {
	f := newFileFixture(t)
	f.mkdir("real")
	f.mkdir("real/sub")
	inside := f.put("real/a.txt", patternBytes(protocol.MaxFilePage+100))
	f.put("real/sub/b.txt", []byte("beta\n"))
	if err := os.Symlink(f.path("real"), f.path("link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, f.path("real/alias.txt")); err != nil {
		t.Fatal(err)
	}
	viaLink := f.path("link/a.txt")
	first := f.readPage(protocol.FileReadRequest{Path: viaLink})
	if first.Path != inside || first.NextCursor == "" {
		t.Fatalf("read reports %q, want the canonical %q", first.Path, inside)
	}
	// The cursor is bound to the canonical path, so the real path continues it
	// and so does the linked one.
	for _, again := range []string{viaLink, inside} {
		if page := f.readPage(protocol.FileReadRequest{Path: again, Cursor: first.NextCursor}); page.Start != first.End {
			t.Fatalf("continuation through %s starts at %d", again, page.Start)
		}
	}
	created := fileOK[protocol.FileWriteResult](t, f.write(protocol.FileWriteRequest{Path: f.path("link/new.txt"), Data: "x", CreateOnly: true}), protocol.TypeFileWriteResult)
	if created.Path != f.path("real/new.txt") {
		t.Fatalf("write reports %q", created.Path)
	}
	f.requireContent(f.path("real/new.txt"), []byte("x"))
	patched := fileOK[protocol.FilePatchResult](t, f.patch(protocol.FilePatchRequest{Path: f.path("link/new.txt"), ExpectedSHA256: created.SHA256, Edits: []protocol.FileEdit{{OldText: "x", NewText: "y"}}}), protocol.TypeFilePatchResult)
	if patched.Path != f.path("real/new.txt") {
		t.Fatalf("patch reports %q", patched.Path)
	}
	list := fileOK[protocol.FileListResult](t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.path("link/sub")}), protocol.TypeFileListResult)
	if list.Path != f.path("real/sub") || len(list.Entries) != 1 {
		t.Fatalf("list reports %q with %d entries", list.Path, len(list.Entries))
	}
	search := f.search(protocol.FileSearchRequest{Path: f.path("link/sub"), Pattern: "beta"})
	if len(search.Matches) != 1 || search.Matches[0].Path != f.path("real/sub/b.txt") {
		t.Fatalf("search: %+v", search.Matches)
	}
	// List and search resolve the whole path, so a linked root works too.
	root := fileOK[protocol.FileListResult](t, f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.path("link")}), protocol.TypeFileListResult)
	if root.Path != f.path("real") || len(root.Entries) != 4 {
		t.Fatalf("list through a linked root reports %q with %d entries", root.Path, len(root.Entries))
	}
	found := f.search(protocol.FileSearchRequest{Path: f.path("link"), Pattern: "beta"})
	if found.Path != f.path("real") || len(found.Matches) != 1 || found.Matches[0].Path != f.path("real/sub/b.txt") {
		t.Fatalf("search through a linked root: %q %+v", found.Path, found.Matches)
	}
	// Reads, writes and patches never follow a symlink as the last element.
	fileCode(t, f.read(protocol.FileReadRequest{Path: f.path("link/alias.txt")}), "file.not_regular")
	fileCode(t, f.read(protocol.FileReadRequest{Path: f.path("missing/a.txt")}), "file.not_found")
}

// A parent swapped for a symlink after the path was resolved is refused, and
// nothing reaches the directory it points at.
func TestFileOperationsRefuseAParentSwappedAfterResolution(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "a.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"read", "write", "patch", "list", "search"} {
		f := newFileFixture(t)
		f.mkdir("real")
		f.put("real/a.txt", []byte("alpha\n"))
		f.controller.hook = func(stage, _ string) {
			if stage != "resolved" {
				return
			}
			if err := os.Rename(f.path("real"), f.path("real.moved")); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, f.path("real")); err != nil {
				t.Error(err)
			}
		}
		path, digest := f.path("real/a.txt"), digestHex([]byte("alpha\n"))
		var response protocol.Control
		switch operation {
		case "read":
			response = f.read(protocol.FileReadRequest{Path: path})
		case "write":
			response = f.write(protocol.FileWriteRequest{Path: f.path("real/new.txt"), Data: "x", CreateOnly: true})
		case "patch":
			response = f.patch(protocol.FilePatchRequest{Path: path, ExpectedSHA256: digest, Edits: []protocol.FileEdit{{OldText: "alpha", NewText: "x"}}})
		case "list":
			response = f.call(protocol.TypeFileList, protocol.FileListRequest{Target: f.target(), Path: f.path("real")})
		default:
			response = f.call(protocol.TypeFileSearch, protocol.FileSearchRequest{Target: f.target(), Path: f.path("real"), Pattern: "outside"})
		}
		fileCode(t, response, "file.not_regular")
		if entries, _ := os.ReadDir(outside); len(entries) != 1 {
			t.Fatalf("%s: the operation reached the directory the parent was swapped for: %v", operation, entries)
		}
		f.requireContent(f.path("real.moved/a.txt"), []byte("alpha\n"))
		requireNoStagedFiles(t, f.path("real.moved"), outside)
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
	// Escape-heavy directory names, as deep as the filesystem's path limit allows.
	deep := f.root
	for range 6 {
		next := filepath.Join(deep, strings.Repeat("<", 200))
		if err := os.Mkdir(next, 0o750); err != nil {
			break
		}
		deep = next
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
	// As deep as the filesystem's path limit allows: PATH_MAX is 4096 on Linux
	// and 1024 on macOS.
	for len(dir)+256 < protocol.MaxFilePath-16 {
		next := filepath.Join(dir, strings.Repeat("<", 250))
		if err := os.Mkdir(next, 0o750); err != nil {
			break
		}
		dir = next
	}
	path := filepath.Join(dir, "data.txt")
	want := patternBytes(protocol.MaxFilePage + 4096)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(path) < 600 {
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
