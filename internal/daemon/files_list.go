package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// maxListedDirectory bounds the names one listing holds in memory.
const maxListedDirectory = 100000

// list returns one page of a directory in name order. The cursor is the last
// name returned, so entries added or removed between pages shift nothing.
func (c *fileController) list(ctx context.Context, request protocol.FileListRequest) (protocol.FileListResult, error) {
	target, dirPath, err := c.beginFileRequest(request.Target, request.Path)
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
		cursor, err := c.decodeCursor(request.Cursor, fileCursorList, dirPath)
		if err != nil {
			return protocol.FileListResult{}, err
		}
		after = cursor.After
	}
	dir, err := openDirectory(dirPath)
	if err != nil {
		return protocol.FileListResult{}, err
	}
	defer dir.Close() //nolint:errcheck // The descriptor is read-only.
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
	result := protocol.FileListResult{Target: target, Path: dirPath, Entries: make([]protocol.FileEntry, 0, len(rest))}
	for _, name := range rest {
		result.Entries = append(result.Entries, describeEntry(dir, name))
	}
	if more {
		result.NextCursor = c.encodeCursor(fileCursor{Kind: fileCursorList, After: rest[len(rest)-1]}, dirPath)
	}
	return result, nil
}

// readDirectoryNames returns the sorted entry names of an opened directory.
func readDirectoryNames(ctx context.Context, dir *os.File) ([]string, error) {
	var names []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("daemon: list cancelled: %w", err)
		}
		batch, err := dir.Readdirnames(4096)
		names = append(names, batch...)
		if len(names) > maxListedDirectory {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: %s has more than %d entries; list a subdirectory or search it", dir.Name(), maxListedDirectory)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fileOSFailure("read directory", dir.Name(), err)
		}
	}
	slices.Sort(names)
	return names, nil
}

func describeEntry(dir *os.File, name string) protocol.FileEntry {
	entry := protocol.FileEntry{Name: name, Kind: protocol.FileKindOther}
	info, err := statAt(dir, name)
	if err != nil {
		return entry // Removed since the listing: report the name and nothing else.
	}
	entry.Kind = entryKind(info)
	if info.regular() {
		entry.Size = info.size
	}
	entry.Mode = fmt.Sprintf("%04o", info.permission()&0o777)
	entry.ModTime = time.Unix(0, info.mtime).UTC().Format(time.RFC3339Nano)
	return entry
}

func entryKind(info fileStat) string {
	switch {
	case info.symlink():
		return protocol.FileKindSymlink
	case info.directory():
		return protocol.FileKindDir
	case info.regular():
		return protocol.FileKindFile
	default:
		return protocol.FileKindOther
	}
}

// searchDepth bounds how deep one search descends, and so how many
// descriptors it holds.
const searchDepth = 64

// searchEnvelope reserves room in the reply for everything but the matches.
const searchEnvelope = 2048

// fileSearch holds the state of one bounded search.
type fileSearch struct {
	controller *fileController
	pattern    *regexp.Regexp
	glob       string
	limit      int
	result     protocol.FileSearchResult
	// entries counts every directory entry looked at, of any kind, against the
	// traversal budget.
	entries int
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
	state, err := newFileSearch(c, request)
	if err != nil {
		return protocol.FileSearchResult{}, err
	}
	state.result.Target, state.result.Path = target, root
	if err := state.run(ctx, root); err != nil && !errors.Is(err, fs.SkipAll) {
		return protocol.FileSearchResult{}, err
	}
	return state.result, nil
}

func newFileSearch(c *fileController, request protocol.FileSearchRequest) (*fileSearch, error) {
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
	return &fileSearch{controller: c, pattern: pattern, glob: request.Glob, limit: limit, reply: searchEnvelope, result: protocol.FileSearchResult{Matches: []protocol.FileMatch{}}}, nil
}

// run searches a directory or one regular file named by root.
func (s *fileSearch) run(ctx context.Context, root string) error {
	if root == "/" {
		dir, err := openDirectory(root)
		if err != nil {
			return err
		}
		defer dir.Close() //nolint:errcheck // The descriptor is read-only.
		return s.walk(ctx, dir, root, 0)
	}
	location, err := locateFile(root)
	if err != nil {
		return err
	}
	defer location.close()
	info, err := statAt(location.dir, location.name)
	if err != nil {
		return fileOSFailure("stat", root, err)
	}
	switch {
	case info.directory():
		dir, err := openChildDirectory(location.dir, location.name, root)
		if err != nil {
			return err
		}
		defer dir.Close() //nolint:errcheck // The descriptor is read-only.
		return s.walk(ctx, dir, root, 0)
	case info.regular():
		s.result.FilesVisited++
		return s.searchFile(location.dir, location.name, root)
	default:
		return fileFailure(agentapi.FileNotRegular, "daemon: %s is %s; search a directory or a regular file", root, info.describe())
	}
}

// walk searches one opened directory. It reads at most the entries the
// traversal budget still allows, so a huge directory costs no more than the
// budget, and it enters children through the descriptor it holds.
func (s *fileSearch) walk(ctx context.Context, dir *os.File, dirPath string, depth int) error {
	remaining := max(protocol.MaxFileSearchFiles-s.entries, 0)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: search cancelled: %w", err)
	}
	// One name more than the budget allows says whether the directory goes on.
	names, err := dir.Readdirnames(remaining + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		s.result.Skipped++
		return nil //nolint:nilerr // an unreadable directory is skipped, not fatal
	}
	s.controller.stage("listed-directory", dirPath)
	if len(names) > remaining {
		s.result.Truncated = protocol.FileTruncatedFiles
		names = names[:remaining]
	}
	slices.Sort(names)
	for _, name := range names {
		if err := s.visit(ctx, dir, dirPath, name, depth); err != nil {
			return err
		}
	}
	if s.result.Truncated != "" {
		return fs.SkipAll
	}
	return nil
}

func (s *fileSearch) visit(ctx context.Context, dir *os.File, dirPath, name string, depth int) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: search cancelled: %w", err)
	}
	s.entries++
	child := path.Join(dirPath, name)
	info, err := statAt(dir, name)
	if err != nil {
		s.result.Skipped++
		return nil //nolint:nilerr // an entry removed or unreadable since the listing is skipped, not fatal
	}
	if info.directory() {
		return s.descend(ctx, dir, name, child, depth)
	}
	s.result.FilesVisited++
	if !info.regular() {
		s.result.Skipped++
		return nil
	}
	if s.glob != "" {
		if matched, _ := path.Match(s.glob, name); !matched {
			return nil
		}
	}
	return s.searchFile(dir, name, child)
}

func (s *fileSearch) descend(ctx context.Context, dir *os.File, name, child string, depth int) error {
	s.result.DirectoriesVisited++
	if depth+1 > searchDepth {
		s.result.Skipped++
		return nil
	}
	s.controller.stage("enter-directory", child)
	sub, err := openChildDirectory(dir, name, child)
	if err != nil {
		s.result.Skipped++ // A link or an unreadable directory: never followed.
		return nil
	}
	defer sub.Close() //nolint:errcheck // The descriptor is read-only.
	return s.walk(ctx, sub, child, depth+1)
}

func (s *fileSearch) searchFile(dir *os.File, name, filePath string) error {
	file, info, err := (fileLocation{dir: dir, name: name, path: filePath}).openRegular()
	if err != nil {
		s.result.Skipped++
		return nil
	}
	defer file.Close() //nolint:errcheck // The descriptor is read-only.
	if info.size > protocol.MaxFileSearchSize {
		s.result.Skipped++
		return nil
	}
	if s.bytes+int64(info.size) > s.controller.searchBytes { //nolint:gosec // bounded by MaxFileSearchSize above
		s.result.Truncated = protocol.FileTruncatedBytes
		return fs.SkipAll
	}
	s.bytes += int64(info.size) //nolint:gosec // bounded by MaxFileSearchSize above
	data := make([]byte, info.size)
	if n, err := file.ReadAt(data, 0); err != nil && !(errors.Is(err, io.EOF) && n == len(data)) {
		s.result.Skipped++
		return nil
	}
	if isBinary(data) {
		s.result.Skipped++
		return nil
	}
	return s.scan(filePath, data)
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
		// The budget counts the serialized match, where escaping can make one
		// character six bytes, plus the separator.
		encoded, err := json.Marshal(match)
		if err != nil {
			return fileFailure(agentapi.InternalError, "daemon: encode match in %s: %w", current, err)
		}
		s.reply += len(encoded) + 1
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
