package mcpnative

import (
	"context"
	"time"
)

// FileControls is the file, copy, fleet and profile half of the controls
// boundary. Controls embeds it.
type FileControls interface {
	FileRead(ctx context.Context, req FileReadRequest) (CallOutcome, error)
	FileList(ctx context.Context, req FileListRequest) (CallOutcome, error)
	FileSearch(ctx context.Context, req FileSearchRequest) (CallOutcome, error)
	FileWrite(ctx context.Context, req FileWriteRequest) (CallOutcome, error)
	FilePatch(ctx context.Context, req FilePatchRequest) (CallOutcome, error)
	Copy(ctx context.Context, req CopyRequest) (CallOutcome, error)
	FleetStatus(ctx context.Context, req FleetStatusRequest) (CallOutcome, error)
	ProfileGet(ctx context.Context, req ProfileGetRequest) (CallOutcome, error)
	ProfileSet(ctx context.Context, req ProfileSetRequest) (CallOutcome, error)
}

// FileTargetRef names where a file request runs: a host, and optionally an
// execution context whose working directory resolves relative paths. The
// handle's host is the host when Host is empty.
type FileTargetRef struct {
	Host    string
	Context *FileContextRef
}

// FileContextRef is a parsed mc1.<hostId>.<contextId> handle.
type FileContextRef struct {
	HostID    string
	ContextID string
}

// String renders the canonical handle.
func (r FileContextRef) String() string {
	return fileContextPrefix + "." + r.HostID + "." + r.ContextID
}

type FileReadRequest struct {
	Target   FileTargetRef
	Path     string
	Offset   uint64
	MaxBytes int
	Cursor   string
	Digest   bool
	Timeout  time.Duration
}

type FileListRequest struct {
	Target  FileTargetRef
	Path    string
	Limit   int
	Cursor  string
	Timeout time.Duration
}

type FileSearchRequest struct {
	Target     FileTargetRef
	Path       string
	Pattern    string
	Glob       string
	MaxMatches int
	Timeout    time.Duration
}

type FileWriteRequest struct {
	Target         FileTargetRef
	Path           string
	Data           []byte
	ExpectedSHA256 string
	CreateOnly     bool
	Timeout        time.Duration
}

// FileEditRequest replaces one exact occurrence of OldText.
type FileEditRequest struct {
	OldText string
	NewText string
}

type FilePatchRequest struct {
	Target         FileTargetRef
	Path           string
	ExpectedSHA256 string
	Edits          []FileEditRequest
	Timeout        time.Duration
}

// Copy directions.
const (
	CopyUpload   = "upload"
	CopyDownload = "download"
)

type CopyRequest struct {
	Host       string
	Direction  string
	LocalPath  string
	RemotePath string
	Recursive  bool
	Timeout    time.Duration
}

type FleetStatusRequest struct{}

// Profile scopes.
const (
	ProfileScopeHost  = "host"
	ProfileScopeFleet = "fleet"
)

type ProfileGetRequest struct {
	Scope string
}

type ProfileSetRequest struct {
	Scope            string
	ExpectedRevision uint64
	Document         []byte
}

// FileReadData is the structured data returned by mesh_file_read. Offsets are
// decimal strings. Encoding is utf-8 when the page's bytes are valid UTF-8
// without NUL and fit the encoded budget, otherwise base64.
type FileReadData struct {
	Target     TargetMeta `json:"target"`
	Path       string     `json:"path"`
	Encoding   string     `json:"encoding"`
	Data       string     `json:"data"`
	Start      string     `json:"start"`
	End        string     `json:"end"`
	Size       string     `json:"size"`
	EOF        bool       `json:"eof"`
	Revision   string     `json:"revision"`
	SHA256     string     `json:"sha256,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// FileEntryData is one directory entry.
type FileEntryData struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    string `json:"size,omitempty"`
	Mode    string `json:"mode,omitempty"`
	ModTime string `json:"modTime,omitempty"`
}

// FileListData is the structured data returned by mesh_file_list.
type FileListData struct {
	Target     TargetMeta      `json:"target"`
	Path       string          `json:"path"`
	Entries    []FileEntryData `json:"entries"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// FileMatchData is one matching line.
type FileMatchData struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

// FileSearchData is the structured data returned by mesh_file_search.
type FileSearchData struct {
	Target             TargetMeta      `json:"target"`
	Path               string          `json:"path"`
	Matches            []FileMatchData `json:"matches"`
	FilesVisited       int             `json:"filesVisited"`
	DirectoriesVisited int             `json:"directoriesVisited"`
	Skipped            int             `json:"skipped"`
	Truncated          string          `json:"truncated,omitempty"`
}

// FileWriteData is the structured data returned by mesh_file_write and
// mesh_file_patch. Revision and SHA256 describe the version this call
// installed.
type FileWriteData struct {
	Target   TargetMeta `json:"target"`
	Path     string     `json:"path"`
	Size     string     `json:"size"`
	SHA256   string     `json:"sha256"`
	Revision string     `json:"revision"`
	Created  bool       `json:"created,omitempty"`
	Edits    int        `json:"edits,omitempty"`
}

// CopyData is the structured data returned by mesh_copy.
type CopyData struct {
	Target     TargetMeta `json:"target"`
	Direction  string     `json:"direction"`
	LocalPath  string     `json:"localPath"`
	RemotePath string     `json:"remotePath"`
	Recursive  bool       `json:"recursive,omitempty"`
	Bytes      string     `json:"bytes"`
}
