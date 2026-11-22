package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// fileController serves the fs.* controls. Every operation runs as the daemon's
// own OS account, so what a caller may touch is what that account may touch.
// Callers must name the exact host and, optionally, the account; the daemon
// refuses a request meant for any other target before it reads a path.
type fileController struct {
	hostID  string
	account string
	// writeMu serializes this daemon's own commits, so two writers that both
	// passed their digest check cannot interleave. Other processes can still
	// change a file between the final check and the rename; the check narrows
	// that window to the time between two system calls and cannot close it.
	writeMu sync.Mutex
}

func newFileController(hostID string) *fileController {
	return &fileController{hostID: hostID, account: daemonAccount()}
}

func daemonAccount() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return strconv.Itoa(os.Getuid())
}

// fileError carries the stable code a client acts on.
type fileError struct {
	code agentapi.Code
	err  error
}

func (e *fileError) Error() string { return e.err.Error() }
func (e *fileError) Unwrap() error { return e.err }

func fileFailure(code agentapi.Code, format string, args ...any) error {
	return &fileError{code: code, err: fmt.Errorf(format, args...)}
}

// fileOSFailure classifies an operating system error and keeps it wrapped.
func fileOSFailure(operation, path string, err error) error {
	var existing *fileError
	if errors.As(err, &existing) {
		return err
	}
	code := agentapi.InternalError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = agentapi.FileNotFound
	case errors.Is(err, fs.ErrPermission):
		code = agentapi.FilePermissionDenied
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENXIO), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EISDIR):
		code = agentapi.FileNotRegular
	}
	return &fileError{code: code, err: fmt.Errorf("daemon: %s %s: %w", operation, path, err)}
}

func fileErrorResponse(request protocol.Control, err error) protocol.Control {
	code := agentapi.InternalError
	var failure *fileError
	if errors.As(err, &failure) {
		code = failure.code
	}
	return protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, ErrorCode: string(code), Message: err.Error()}
}

func (c *fileController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if !protocol.IsFileControl(request.Type) {
		return protocol.Control{}, false, nil
	}
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, true, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, true, fmt.Errorf("daemon: %s cancelled: %w", request.Type, err)
	}
	resultType, result, err := c.dispatch(ctx, request)
	if err != nil {
		return fileErrorResponse(request, err), true, nil
	}
	data, err := protocol.EncodeFileDocument(result)
	if err != nil {
		return fileErrorResponse(request, err), true, nil
	}
	return protocol.Control{Type: resultType, RequestID: request.RequestID, Files: data}, true, nil
}

func (c *fileController) dispatch(ctx context.Context, request protocol.Control) (string, any, error) {
	switch request.Type {
	case protocol.TypeFileRead:
		var body protocol.FileReadRequest
		if err := decodeFileRequest(request, &body); err != nil {
			return "", nil, err
		}
		result, err := c.read(ctx, body)
		return protocol.TypeFileReadResult, result, err
	case protocol.TypeFileList:
		var body protocol.FileListRequest
		if err := decodeFileRequest(request, &body); err != nil {
			return "", nil, err
		}
		result, err := c.list(ctx, body)
		return protocol.TypeFileListResult, result, err
	case protocol.TypeFileSearch:
		var body protocol.FileSearchRequest
		if err := decodeFileRequest(request, &body); err != nil {
			return "", nil, err
		}
		result, err := c.search(ctx, body)
		return protocol.TypeFileSearchResult, result, err
	case protocol.TypeFileWrite:
		var body protocol.FileWriteRequest
		if err := decodeFileRequest(request, &body); err != nil {
			return "", nil, err
		}
		result, err := c.write(ctx, body)
		return protocol.TypeFileWriteResult, result, err
	default:
		var body protocol.FilePatchRequest
		if err := decodeFileRequest(request, &body); err != nil {
			return "", nil, err
		}
		result, err := c.patch(ctx, body)
		return protocol.TypeFilePatchResult, result, err
	}
}

func decodeFileRequest(request protocol.Control, body any) error {
	if err := protocol.DecodeFileDocument(request.Files, body); err != nil {
		return fileFailure(agentapi.UsageInvalid, "daemon: %s request: %w", request.Type, err)
	}
	return nil
}

// checkTarget refuses a request addressed to another host or OS account.
func (c *fileController) checkTarget(target protocol.FileTarget) (protocol.FileTarget, error) {
	if target.HostID != c.hostID {
		return protocol.FileTarget{}, fileFailure(agentapi.HostIdentityMismatch, "daemon: file request is for host %q, this host is %q", target.HostID, c.hostID)
	}
	if target.Account != "" && target.Account != c.account {
		return protocol.FileTarget{}, fileFailure(agentapi.FileTargetMismatch, "daemon: file request is for account %q, the daemon runs as %q", target.Account, c.account)
	}
	return protocol.FileTarget{HostID: c.hostID, Account: c.account}, nil
}

// resolveFilePath turns a request path into the absolute path the operation
// uses. Only absolute paths are accepted today. A later change resolves a
// relative path against an authorized context's working directory here, after
// the target check and before any file access, and nowhere else.
func resolveFilePath(path string) (string, error) {
	if path == "" || len(path) > protocol.MaxFilePath || strings.ContainsRune(path, 0) {
		return "", fileFailure(agentapi.UsageInvalid, "daemon: file path must be 1 to %d bytes without NUL", protocol.MaxFilePath)
	}
	if !filepath.IsAbs(path) {
		return "", fileFailure(agentapi.UsageInvalid, "daemon: file path %q must be absolute", path)
	}
	return filepath.Clean(path), nil
}

// beginFileRequest runs the checks every operation shares.
func (c *fileController) beginFileRequest(target protocol.FileTarget, path string) (protocol.FileTarget, string, error) {
	checked, err := c.checkTarget(target)
	if err != nil {
		return protocol.FileTarget{}, "", err
	}
	resolved, err := resolveFilePath(path)
	if err != nil {
		return protocol.FileTarget{}, "", err
	}
	return checked, resolved, nil
}

// fileCursor is the opaque continuation token. It restates the binding the
// daemon checks again on every use, so a token from another host, account or
// path, or for a file that changed, is refused. It grants no access the path
// does not already give.
type fileCursor struct {
	Kind     string `json:"k"`
	Host     string `json:"h"`
	Account  string `json:"a"`
	Path     string `json:"p"`
	Offset   uint64 `json:"o,omitempty"`
	Revision string `json:"r,omitempty"`
	After    string `json:"n,omitempty"`
}

const (
	fileCursorRead = "r"
	fileCursorList = "l"
	maxFileCursor  = 16 << 10
)

func (c *fileController) encodeCursor(cursor fileCursor) string {
	cursor.Host, cursor.Account = c.hostID, c.account
	data, _ := json.Marshal(cursor) // A struct of strings and integers always encodes.
	return base64.RawURLEncoding.EncodeToString(data)
}

func (c *fileController) decodeCursor(token, kind, path string) (fileCursor, error) {
	var cursor fileCursor
	if len(token) > maxFileCursor {
		return cursor, fileFailure(agentapi.FileCursorInvalid, "daemon: file cursor is too long")
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || json.Unmarshal(data, &cursor) != nil {
		return fileCursor{}, fileFailure(agentapi.FileCursorInvalid, "daemon: file cursor is malformed")
	}
	if cursor.Kind != kind || cursor.Host != c.hostID || cursor.Account != c.account || cursor.Path != path {
		return fileCursor{}, fileFailure(agentapi.FileCursorInvalid, "daemon: file cursor belongs to another host, account, path or operation")
	}
	return cursor, nil
}

// openRegularFile opens path for reading and refuses anything that is not a
// regular file, including a symlink in the last path element. The check runs
// on the open descriptor, so a swap between the path check and the open cannot
// substitute a special file, and O_NONBLOCK keeps a swapped-in FIFO from
// blocking the open.
func openRegularFile(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fileOSFailure("stat", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fileFailure(agentapi.FileNotRegular, "daemon: %s is %s, not a regular file", path, describeMode(before.Mode()))
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0) //nolint:gosec // the caller names the file; the daemon account's own permissions decide access
	if err != nil {
		return nil, nil, fileOSFailure("open", path, err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, nil, fileOSFailure("stat", path, err)
		}
		return nil, nil, fileFailure(agentapi.FileNotRegular, "daemon: %s is %s, not a regular file", path, describeMode(info.Mode()))
	}
	return file, info, nil
}

func describeMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symlink"
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0:
		return "a device"
	default:
		return "a special file"
	}
}

// fileRevision fingerprints identity and content-change state. Replacing the
// file changes the inode, and writing it in place changes size, modification
// time or change time.
func fileRevision(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return protocol.FileRevisionString(0, 0, uint64(info.Size()), info.ModTime().UnixNano(), 0) //nolint:gosec // a file size is never negative
	}
	return protocol.FileRevisionString(uint64(stat.Dev), uint64(stat.Ino), uint64(stat.Size), info.ModTime().UnixNano(), statChangeNanos(stat)) //nolint:gosec // device, inode and size are fingerprint bits, not quantities
}

// hashFile streams a regular file through SHA-256, leaving the descriptor
// position alone, and stops when the caller goes away.
func hashFile(ctx context.Context, file *os.File, size int64) (string, error) {
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("daemon: hash cancelled: %w", err)
		}
		n, err := file.ReadAt(buffer[:min(int64(len(buffer)), size-offset)], offset)
		digest.Write(buffer[:n])
		offset += int64(n)
		if err != nil && !(errors.Is(err, io.EOF) && offset == size) {
			return "", fileOSFailure("read", file.Name(), err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
