package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// maxListedDirectory bounds the names one listing holds in memory.
const maxListedDirectory = 100000

// list returns one page of a directory in name order. The cursor is the last
// name returned, so entries added or removed between pages shift nothing.
func (c *fileController) list(ctx context.Context, request protocol.FileListRequest) (protocol.FileListResult, error) {
	target, dir, err := c.beginFileRequest(request.Target, request.Path)
	if err != nil {
		return protocol.FileListResult{}, err
	}
	limit := request.Limit
	if limit < 0 || limit > protocol.MaxFileList {
		return protocol.FileListResult{}, fileFailure(agentapi.UsageInvalid, "daemon: limit must be between 0 and %d", protocol.MaxFileList)
	}
	if limit == 0 {
		limit = protocol.DefaultFileList
	}
	after := ""
	if request.Cursor != "" {
		cursor, err := c.decodeCursor(request.Cursor, fileCursorList, dir)
		if err != nil {
			return protocol.FileListResult{}, err
		}
		after = cursor.After
	}
	names, err := readDirectoryNames(ctx, dir)
	if err != nil {
		return protocol.FileListResult{}, err
	}
	start, _ := slices.BinarySearch(names, after)
	if after != "" && start < len(names) && names[start] == after {
		start++
	}
	rest := names[start:]
	more := len(rest) > limit
	if more {
		rest = rest[:limit]
	}
	result := protocol.FileListResult{Target: target, Path: dir, Entries: make([]protocol.FileEntry, 0, len(rest))}
	for _, name := range rest {
		result.Entries = append(result.Entries, describeEntry(dir, name))
	}
	if more {
		result.NextCursor = c.encodeCursor(fileCursor{Kind: fileCursorList, Path: dir, After: rest[len(rest)-1]})
	}
	return result, nil
}

// readDirectoryNames opens dir without following a symlink and returns its
// sorted entry names.
func readDirectoryNames(ctx context.Context, dir string) ([]string, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, fileOSFailure("stat", dir, err)
	}
	if !before.IsDir() {
		return nil, fileFailure(agentapi.FileNotRegular, "daemon: %s is %s, not a directory", dir, describeMode(before.Mode()))
	}
	handle, err := os.OpenFile(dir, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0) //nolint:gosec // the caller names the directory; the daemon account's own permissions decide access
	if err != nil {
		return nil, fileOSFailure("open", dir, err)
	}
	defer handle.Close() //nolint:errcheck // The descriptor is read-only.
	var names []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("daemon: list cancelled: %w", err)
		}
		batch, err := handle.Readdirnames(4096)
		names = append(names, batch...)
		if len(names) > maxListedDirectory {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: %s has more than %d entries; list a subdirectory or search it", dir, maxListedDirectory)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fileOSFailure("read directory", dir, err)
		}
	}
	slices.Sort(names)
	return names, nil
}

func describeEntry(dir, name string) protocol.FileEntry {
	entry := protocol.FileEntry{Name: name, Kind: protocol.FileKindOther}
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return entry // Removed since the listing: report the name and nothing else.
	}
	entry.Kind = entryKind(info.Mode())
	if info.Mode().IsRegular() {
		entry.Size = uint64(info.Size()) //nolint:gosec // a file size is never negative
	}
	entry.Mode = fmt.Sprintf("%04o", info.Mode().Perm())
	entry.ModTime = info.ModTime().UTC().Format(time.RFC3339Nano)
	return entry
}

func entryKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return protocol.FileKindSymlink
	case mode.IsDir():
		return protocol.FileKindDir
	case mode.IsRegular():
		return protocol.FileKindFile
	default:
		return protocol.FileKindOther
	}
}

// fileSearch holds the state of one bounded search.
type fileSearch struct {
	root    string
	pattern *regexp.Regexp
	glob    string
	limit   int
	result  protocol.FileSearchResult
	bytes   int64
	reply   int
}

// search walks the tree under the path with Go's regexp package. It never
// starts a process, never follows a symlink and stops at the documented bounds,
// naming the bound it hit.
func (c *fileController) search(ctx context.Context, request protocol.FileSearchRequest) (protocol.FileSearchResult, error) {
	target, root, err := c.beginFileRequest(request.Target, request.Path)
	if err != nil {
		return protocol.FileSearchResult{}, err
	}
	state, err := newFileSearch(request)
	if err != nil {
		return protocol.FileSearchResult{}, err
	}
	state.root, state.result.Target, state.result.Path = root, target, root
	info, err := os.Lstat(root)
	if err != nil {
		return protocol.FileSearchResult{}, fileOSFailure("stat", root, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !(info.IsDir() || info.Mode().IsRegular()) {
		return protocol.FileSearchResult{}, fileFailure(agentapi.FileNotRegular, "daemon: %s is %s; search a directory or a regular file", root, describeMode(info.Mode()))
	}
	walkErr := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		return state.visit(ctx, current, entry, err)
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return protocol.FileSearchResult{}, fmt.Errorf("daemon: search %s: %w", root, walkErr)
	}
	return state.result, nil
}

func newFileSearch(request protocol.FileSearchRequest) (*fileSearch, error) {
	if request.Pattern == "" || len(request.Pattern) > protocol.MaxFilePattern || len(request.Glob) > protocol.MaxFilePattern {
		return nil, fileFailure(agentapi.UsageInvalid, "daemon: pattern must be 1 to %d bytes", protocol.MaxFilePattern)
	}
	pattern, err := regexp.Compile(request.Pattern)
	if err != nil {
		return nil, fileFailure(agentapi.UsageInvalid, "daemon: search pattern: %w", err)
	}
	if _, err := path.Match(request.Glob, ""); err != nil {
		return nil, fileFailure(agentapi.UsageInvalid, "daemon: search glob: %w", err)
	}
	if request.MaxMatches < 0 || request.MaxMatches > protocol.MaxFileSearchMatches {
		return nil, fileFailure(agentapi.UsageInvalid, "daemon: maxMatches must be between 0 and %d", protocol.MaxFileSearchMatches)
	}
	limit := request.MaxMatches
	if limit == 0 {
		limit = protocol.MaxFileSearchMatches
	}
	return &fileSearch{pattern: pattern, glob: request.Glob, limit: limit, result: protocol.FileSearchResult{Matches: []protocol.FileMatch{}}}, nil
}

func (s *fileSearch) visit(ctx context.Context, current string, entry fs.DirEntry, walkErr error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: search cancelled: %w", err)
	}
	if walkErr != nil {
		if current == s.root {
			return fileOSFailure("read", current, walkErr)
		}
		return s.unreadable(entry)
	}
	if entry.IsDir() {
		return nil
	}
	if s.result.FilesVisited >= protocol.MaxFileSearchFiles {
		s.result.Truncated = protocol.FileTruncatedFiles
		return fs.SkipAll
	}
	s.result.FilesVisited++
	if s.glob != "" {
		if matched, _ := path.Match(s.glob, entry.Name()); !matched {
			return nil
		}
	}
	if !entry.Type().IsRegular() {
		s.result.Skipped++
		return nil
	}
	return s.searchFile(current)
}

// unreadable skips what the daemon account cannot read instead of failing the
// whole search.
func (s *fileSearch) unreadable(entry fs.DirEntry) error {
	if entry != nil && entry.IsDir() {
		return fs.SkipDir
	}
	s.result.Skipped++
	return nil
}

func (s *fileSearch) searchFile(current string) error {
	file, info, err := openRegularFile(current)
	if err != nil {
		s.result.Skipped++
		return nil
	}
	defer file.Close() //nolint:errcheck // The descriptor is read-only.
	if info.Size() > protocol.MaxFileSearchSize {
		s.result.Skipped++
		return nil
	}
	if s.bytes+info.Size() > protocol.MaxFileSearchBytes {
		s.result.Truncated = protocol.FileTruncatedBytes
		return fs.SkipAll
	}
	s.bytes += info.Size()
	data := make([]byte, info.Size())
	if n, err := file.ReadAt(data, 0); err != nil && !(errors.Is(err, io.EOF) && n == len(data)) {
		s.result.Skipped++
		return nil
	}
	if isBinary(data) {
		s.result.Skipped++
		return nil
	}
	return s.scan(current, data)
}

// isBinary applies the usual heuristic: a NUL in the first 8000 bytes.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

func (s *fileSearch) scan(current string, data []byte) error {
	for number, line := 1, []byte(nil); len(data) > 0; number++ {
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		location := s.pattern.FindIndex(line)
		if location == nil {
			continue
		}
		// Truncation is reported only when one more match exists, so a search
		// that found exactly the limit is complete.
		if len(s.result.Matches) >= s.limit {
			s.result.Truncated = protocol.FileTruncatedMatches
			return fs.SkipAll
		}
		match := protocol.FileMatch{Path: current, Line: number, Column: location[0] + 1, Text: matchText(line)}
		s.reply += len(match.Path) + len(match.Text) + 48
		if s.reply > protocol.MaxFileSearchReply {
			s.result.Truncated = protocol.FileTruncatedReply
			return fs.SkipAll
		}
		s.result.Matches = append(s.result.Matches, match)
	}
	return nil
}

// matchText bounds one line and keeps it valid UTF-8 so JSON carries it intact.
func matchText(line []byte) string {
	if len(line) > protocol.MaxFileMatchText {
		line = trimPartialRune(line[:protocol.MaxFileMatchText])
	}
	return strings.ToValidUTF8(string(line), "�")
}
