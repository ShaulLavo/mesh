package scp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootAppendPreservesDirectoryBoundaries(t *testing.T) {
	root := &RootEntry{}
	assertAppendDirectoryBoundaries(t, root)
}

func TestDirAppendPreservesDirectoryBoundaries(t *testing.T) {
	root := &DirEntry{Filepath: "tree"}
	assertAppendDirectoryBoundaries(t, root)
}

func assertAppendDirectoryBoundaries(t *testing.T, root AppendableEntry) {
	t.Helper()
	a := &DirEntry{Filepath: filepath.Join("tree", "a")}
	ab := &DirEntry{Filepath: filepath.Join("tree", "ab")}
	abc := &DirEntry{Filepath: filepath.Join("tree", "abc")}
	nested := &DirEntry{Filepath: filepath.Join("tree", "ab", "nested")}
	for _, entry := range []*DirEntry{a, ab, nested, abc} {
		root.Append(entry)
	}
	for _, directory := range []*DirEntry{a, ab, nested, abc} {
		root.Append(&FileEntry{Filepath: filepath.Join(directory.Filepath, "value.txt")})
	}
	if len(a.Children) != 1 || len(ab.Children) != 2 || len(nested.Children) != 1 || len(abc.Children) != 1 {
		t.Fatalf("child counts: a=%d, ab=%d, nested=%d, abc=%d", len(a.Children), len(ab.Children), len(nested.Children), len(abc.Children))
	}
	if ab.Children[0] != nested {
		t.Fatalf("ab's first child = %v; want nested directory", ab.Children[0])
	}
	for _, directory := range []*DirEntry{a, ab, nested, abc} {
		last := directory.Children[len(directory.Children)-1]
		if last.path() != filepath.Join(directory.Filepath, "value.txt") {
			t.Fatalf("%q's file = %q", directory.Filepath, last.path())
		}
	}
}

func TestRootAppendKeepsDescendantsUnderFilesystemRoot(t *testing.T) {
	root := &RootEntry{}
	directory := &DirEntry{Filepath: filepath.VolumeName(os.TempDir()) + string(filepath.Separator)}
	child := &DirEntry{Filepath: filepath.Join(directory.Filepath, "a")}
	nested := &DirEntry{Filepath: filepath.Join(child.Filepath, "nested")}
	for _, entry := range []*DirEntry{directory, child, nested} {
		root.Append(entry)
	}
	if len(*root) != 1 || len(directory.Children) != 1 || len(child.Children) != 1 || child.Children[0] != nested {
		t.Fatalf("filesystem root descendants were misplaced: root=%v, children=%v, nested=%v", *root, directory.Children, child.Children)
	}
}
