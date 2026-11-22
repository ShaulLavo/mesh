package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// File control type names. Each request carries one JSON document in
// Control.Files and is answered by the matching result type or by TypeError.
const (
	TypeFileRead         = "fs.read"
	TypeFileReadResult   = "fs.read.result"
	TypeFileList         = "fs.list"
	TypeFileListResult   = "fs.list.result"
	TypeFileSearch       = "fs.search"
	TypeFileSearchResult = "fs.search.result"
	TypeFileWrite        = "fs.write"
	TypeFileWriteResult  = "fs.write.result"
	TypeFilePatch        = "fs.patch"
	TypeFilePatchResult  = "fs.patch.result"
)

// File operation limits. The daemon enforces them; clients use them to size
// requests before sending.
const (
	// MaxFilePage is the most raw bytes one fs.read page returns.
	MaxFilePage = 1 << 20
	// MaxFileWrite is the most raw bytes one fs.write request carries.
	MaxFileWrite = 1 << 20
	// MaxFileTextEncoded bounds a text page after JSON escaping. A page whose
	// escaped text is larger is returned as Base64, which grows by one third.
	MaxFileTextEncoded = 2 << 20
	// MaxFilePatchSize is the largest file fs.patch reads and rewrites.
	MaxFilePatchSize = 4 << 20
	// MaxFilePatchEdits bounds the edits in one patch.
	MaxFilePatchEdits = 64
	// MaxFileDigestSize is the largest file fs.read hashes without being asked.
	MaxFileDigestSize = 4 << 20
	// MaxFileList is the most entries one fs.list page returns.
	MaxFileList = 1000
	// DefaultFileList is the page size when a list request names none.
	DefaultFileList = 200
	// MaxFileSearchFiles is the most files one search visits.
	MaxFileSearchFiles = 10000
	// MaxFileSearchMatches is the most matches one search returns.
	MaxFileSearchMatches = 1000
	// MaxFileSearchSize skips files larger than this.
	MaxFileSearchSize = 4 << 20
	// MaxFileSearchBytes bounds the bytes one search reads, so a tree of
	// maximum-size files cannot hold the connection for minutes.
	MaxFileSearchBytes = 256 << 20
	// MaxFileSearchReply bounds the encoded matches in one search reply.
	MaxFileSearchReply = 1 << 20
	// MaxFileMatchText truncates one matching line.
	MaxFileMatchText = 1024
	// MaxFilePattern bounds a search pattern or glob.
	MaxFilePattern = 4096
	// MaxFilePath bounds an absolute path.
	MaxFilePath = 4096
)

// File encodings.
const (
	FileEncodingUTF8   = "utf-8"
	FileEncodingBase64 = "base64"
)

// Search truncation reasons.
const (
	FileTruncatedFiles   = "files_limit"
	FileTruncatedMatches = "matches_limit"
	FileTruncatedBytes   = "bytes_limit"
	FileTruncatedReply   = "reply_limit"
)

// File entry kinds reported by fs.list.
const (
	FileKindFile    = "file"
	FileKindDir     = "dir"
	FileKindSymlink = "symlink"
	FileKindOther   = "other"
)

// FileTarget names the exact host and OS account a file request is for. The
// daemon refuses a request addressed to any other host or account.
type FileTarget struct {
	HostID string `json:"hostId"`
	// Account is optional on requests and always present on results.
	Account string `json:"account,omitempty"`
}

// FileReadRequest asks for one page of a regular file. A continuation sends
// the previous reply's Cursor and no Offset.
type FileReadRequest struct {
	Target FileTarget `json:"target"`
	Path   string     `json:"path"`
	Offset uint64     `json:"offset,string,omitempty"`
	// MaxBytes defaults to and is capped at MaxFilePage.
	MaxBytes int    `json:"maxBytes,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	// Digest asks for the whole-file SHA-256 of a file larger than
	// MaxFileDigestSize. Smaller files always carry it.
	Digest bool `json:"digest,omitempty"`
}

// FileReadResult is one page. Start and End are byte offsets, End exclusive.
type FileReadResult struct {
	Target   FileTarget `json:"target"`
	Path     string     `json:"path"`
	Encoding string     `json:"encoding"`
	Data     string     `json:"data"`
	Start    uint64     `json:"start,string"`
	End      uint64     `json:"end,string"`
	Size     uint64     `json:"size,string"`
	EOF      bool       `json:"eof"`
	// Revision is opaque. It changes when the file is replaced or changed in
	// place.
	Revision   string `json:"revision"`
	SHA256     string `json:"sha256,omitempty"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// FileListRequest lists one directory in name order.
type FileListRequest struct {
	Target FileTarget `json:"target"`
	Path   string     `json:"path"`
	Limit  int        `json:"limit,omitempty"`
	Cursor string     `json:"cursor,omitempty"`
}

// FileEntry describes one directory entry without following symlinks.
type FileEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    uint64 `json:"size,string,omitempty"`
	Mode    string `json:"mode,omitempty"`
	ModTime string `json:"modTime,omitempty"`
}

// FileListResult is one page of a directory.
type FileListResult struct {
	Target     FileTarget  `json:"target"`
	Path       string      `json:"path"`
	Entries    []FileEntry `json:"entries"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// FileSearchRequest looks for a Go regular expression in the text files under
// Path, which is a directory or one regular file.
type FileSearchRequest struct {
	Target     FileTarget `json:"target"`
	Path       string     `json:"path"`
	Pattern    string     `json:"pattern"`
	Glob       string     `json:"glob,omitempty"`
	MaxMatches int        `json:"maxMatches,omitempty"`
}

// FileMatch is one matching line.
type FileMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

// FileSearchResult reports matches and why a search stopped early.
type FileSearchResult struct {
	Target       FileTarget  `json:"target"`
	Path         string      `json:"path"`
	Matches      []FileMatch `json:"matches"`
	FilesVisited int         `json:"filesVisited"`
	// Skipped counts files not searched: too large, binary, unreadable,
	// symlinks and special files.
	Skipped   int    `json:"skipped"`
	Truncated string `json:"truncated,omitempty"`
}

// FileWriteRequest replaces or creates a file. Exactly one of ExpectedSHA256
// and CreateOnly is required, and the daemon checks it at commit.
type FileWriteRequest struct {
	Target         FileTarget `json:"target"`
	Path           string     `json:"path"`
	Encoding       string     `json:"encoding,omitempty"`
	Data           string     `json:"data"`
	ExpectedSHA256 string     `json:"expectedSha256,omitempty"`
	CreateOnly     bool       `json:"createOnly,omitempty"`
}

// FileWriteResult describes the committed file.
type FileWriteResult struct {
	Target   FileTarget `json:"target"`
	Path     string     `json:"path"`
	Size     uint64     `json:"size,string"`
	SHA256   string     `json:"sha256"`
	Revision string     `json:"revision"`
	Created  bool       `json:"created"`
}

// FileEdit replaces one exact occurrence of OldText.
type FileEdit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// FilePatchRequest applies edits in order to a file whose SHA-256 matches.
type FilePatchRequest struct {
	Target         FileTarget `json:"target"`
	Path           string     `json:"path"`
	ExpectedSHA256 string     `json:"expectedSha256"`
	Edits          []FileEdit `json:"edits"`
}

// FilePatchResult describes the committed file.
type FilePatchResult struct {
	Target   FileTarget `json:"target"`
	Path     string     `json:"path"`
	Size     uint64     `json:"size,string"`
	SHA256   string     `json:"sha256"`
	Revision string     `json:"revision"`
	Edits    int        `json:"edits"`
}

// IsFileControl reports whether name is one of the file request types.
func IsFileControl(name string) bool {
	switch name {
	case TypeFileRead, TypeFileList, TypeFileSearch, TypeFileWrite, TypeFilePatch:
		return true
	default:
		return false
	}
}

// EncodeFileDocument marshals one request or result for Control.Files.
func EncodeFileDocument(document any) (json.RawMessage, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode file document: %w", err)
	}
	return data, nil
}

// DecodeFileDocument parses one request or result strictly: unknown fields,
// duplicate trailing values and a missing document are errors.
func DecodeFileDocument(raw json.RawMessage, document any) error {
	if len(raw) == 0 {
		return fmt.Errorf("protocol: file document is missing")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(document); err != nil {
		return fmt.Errorf("protocol: decode file document: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("protocol: file document has trailing data")
	}
	return nil
}

// FileRevisionString formats a file fingerprint so every component shares one
// spelling.
func FileRevisionString(device, inode, size uint64, modifiedNanos, changedNanos int64) string {
	return strconv.FormatUint(device, 16) + "." + strconv.FormatUint(inode, 16) + "." + strconv.FormatUint(size, 16) + "." +
		strconv.FormatInt(modifiedNanos, 16) + "." + strconv.FormatInt(changedNanos, 16)
}
