package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// newFileMode is the mode of a file fs.write creates. An existing file keeps
// its own permission bits.
const newFileMode fs.FileMode = 0o644

// renameNoReplace moves from to to and fails with fs.ErrExist when to exists,
// as one atomic step. It is the same primitive that publishes Unix sockets.
func renameNoReplace(from, to string) error { return publishUnixSocket(from, to) }

// write replaces or creates one regular file through a temporary file in the
// same directory and a rename, so a reader sees the old or the new bytes and
// never a mix. The caller's precondition is checked again at commit.
func (c *fileController) write(ctx context.Context, request protocol.FileWriteRequest) (protocol.FileWriteResult, error) {
	target, path, err := c.beginFileRequest(request.Target, request.Path)
	if err != nil {
		return protocol.FileWriteResult{}, err
	}
	data, err := decodeWriteData(request)
	if err != nil {
		return protocol.FileWriteResult{}, err
	}
	expected, err := writePrecondition(request)
	if err != nil {
		return protocol.FileWriteResult{}, err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	var plan commitPlan
	if request.CreateOnly {
		plan = commitPlan{mode: newFileMode, noReplace: true}
	} else if plan, err = replacePlan(ctx, path, expected); err != nil {
		return protocol.FileWriteResult{}, err
	}
	defer plan.close()
	if err := commitFile(path, data, plan); err != nil {
		return protocol.FileWriteResult{}, err
	}
	result := protocol.FileWriteResult{Target: target, Path: path, Size: uint64(len(data)), SHA256: digestHex(data), Created: request.CreateOnly}
	if result.Revision, err = committedRevision(path); err != nil {
		return protocol.FileWriteResult{}, err
	}
	return result, nil
}

func decodeWriteData(request protocol.FileWriteRequest) ([]byte, error) {
	switch request.Encoding {
	case "", protocol.FileEncodingUTF8:
		if len(request.Data) > protocol.MaxFileWrite {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: write data exceeds %d bytes; use mesh cp for larger files", protocol.MaxFileWrite)
		}
		return []byte(request.Data), nil
	case protocol.FileEncodingBase64:
		if base64.StdEncoding.DecodedLen(len(request.Data)) > protocol.MaxFileWrite+3 {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: write data exceeds %d bytes; use mesh cp for larger files", protocol.MaxFileWrite)
		}
		data, err := base64.StdEncoding.DecodeString(request.Data)
		if err != nil {
			return nil, fileFailure(agentapi.UsageInvalid, "daemon: write data is not valid Base64: %w", err)
		}
		if len(data) > protocol.MaxFileWrite {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: write data exceeds %d bytes; use mesh cp for larger files", protocol.MaxFileWrite)
		}
		return data, nil
	default:
		return nil, fileFailure(agentapi.UsageInvalid, "daemon: encoding must be %q or %q", protocol.FileEncodingUTF8, protocol.FileEncodingBase64)
	}
}

// writePrecondition requires exactly one of the two commit checks.
func writePrecondition(request protocol.FileWriteRequest) (string, error) {
	if request.CreateOnly == (request.ExpectedSHA256 != "") {
		return "", fileFailure(agentapi.UsageInvalid, "daemon: write needs exactly one of expectedSha256 and createOnly")
	}
	if request.CreateOnly {
		return "", nil
	}
	return normalizeDigest(request.ExpectedSHA256)
}

func normalizeDigest(digest string) (string, error) {
	lower := strings.ToLower(digest)
	if raw, err := hex.DecodeString(lower); err != nil || len(raw) != sha256.Size {
		return "", fileFailure(agentapi.UsageInvalid, "daemon: expectedSha256 must be 64 hexadecimal characters")
	}
	return lower, nil
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// commitPlan is what the final check compares against. A replacement keeps
// the open descriptor and fingerprint of the version whose digest matched.
type commitPlan struct {
	mode      fs.FileMode
	noReplace bool
	current   *os.File
	revision  string
	path      string
}

func (p commitPlan) close() {
	if p.current != nil {
		_ = p.current.Close()
	}
}

// replacePlan verifies the digest of the file being replaced and records the
// fingerprint of the version it hashed.
func replacePlan(ctx context.Context, path, expected string) (commitPlan, error) {
	file, info, err := openRegularFile(path)
	if err != nil {
		return commitPlan{}, err
	}
	plan := commitPlan{mode: info.Mode().Perm(), current: file, path: path}
	actual, err := hashFile(ctx, file, info.Size())
	if err != nil {
		plan.close()
		return commitPlan{}, err
	}
	if actual != expected {
		plan.close()
		return commitPlan{}, fileFailure(agentapi.FileConflict, "daemon: %s has sha256 %s, not the expected %s; read it again", path, actual, expected)
	}
	after, err := file.Stat()
	if err != nil {
		plan.close()
		return commitPlan{}, fileOSFailure("stat", path, err)
	}
	plan.revision = fileRevision(after)
	if plan.revision != fileRevision(info) {
		plan.close()
		return commitPlan{}, fileFailure(agentapi.FileConflict, "daemon: %s changed while its digest was checked; read it again", path)
	}
	return plan, nil
}

// commitFile writes data next to path and renames it into place after the
// plan's last check. The temporary file never survives a failure.
func commitFile(path string, data []byte, plan commitPlan) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".mesh-fs-*")
	if err != nil {
		return fileOSFailure("create temporary file in", dir, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(temp.Name())
		}
	}()
	if err := writeTemp(temp, data, plan.mode); err != nil {
		return fileOSFailure("write", temp.Name(), err)
	}
	if err := plan.verify(); err != nil {
		return err
	}
	if plan.noReplace {
		err = renameNoReplace(temp.Name(), path)
	} else {
		err = os.Rename(temp.Name(), path)
	}
	if errors.Is(err, fs.ErrExist) {
		return fileFailure(agentapi.FileConflict, "daemon: %s already exists", path)
	}
	if err != nil {
		return fileOSFailure("rename into", path, err)
	}
	renamed = true
	syncDirectory(dir)
	return nil
}

func writeTemp(temp *os.File, data []byte, mode fs.FileMode) error {
	_, err := temp.Write(data)
	if err == nil {
		err = temp.Chmod(mode)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	return err
}

// verify is the commit-time check: the path must still name the version the
// digest was checked against.
func (p commitPlan) verify() error {
	if p.current == nil {
		return nil
	}
	info, err := os.Lstat(p.path)
	if err != nil {
		return fileFailure(agentapi.FileConflict, "daemon: %s changed before the write committed: %w", p.path, err)
	}
	if !info.Mode().IsRegular() || fileRevision(info) != p.revision {
		return fileFailure(agentapi.FileConflict, "daemon: %s changed before the write committed; read it again", p.path)
	}
	return nil
}

// syncDirectory makes the rename durable. A filesystem that cannot sync a
// directory has already accepted the rename, so a failure here is not one.
func syncDirectory(dir string) {
	handle, err := os.Open(dir) //nolint:gosec // dir is the parent of the file just committed
	if err != nil {
		return
	}
	_ = handle.Sync()
	_ = handle.Close()
}

func committedRevision(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fileOSFailure("stat", path, err)
	}
	return fileRevision(info), nil
}

// patch applies exact text edits to a file whose digest matches. Each OldText
// must occur exactly once in the text as the earlier edits left it, so an
// ambiguous or stale edit fails instead of landing in the wrong place.
func (c *fileController) patch(ctx context.Context, request protocol.FilePatchRequest) (protocol.FilePatchResult, error) {
	target, path, err := c.beginFileRequest(request.Target, request.Path)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	expected, err := normalizeDigest(request.ExpectedSHA256)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	if len(request.Edits) == 0 || len(request.Edits) > protocol.MaxFilePatchEdits {
		return protocol.FilePatchResult{}, fileFailure(agentapi.UsageInvalid, "daemon: a patch needs 1 to %d edits", protocol.MaxFilePatchEdits)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	plan, content, err := readForPatch(ctx, path, expected)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	defer plan.close()
	patched, err := applyEdits(content, request.Edits)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	if err := commitFile(path, patched, plan); err != nil {
		return protocol.FilePatchResult{}, err
	}
	result := protocol.FilePatchResult{Target: target, Path: path, Size: uint64(len(patched)), SHA256: digestHex(patched), Edits: len(request.Edits)}
	if result.Revision, err = committedRevision(path); err != nil {
		return protocol.FilePatchResult{}, err
	}
	return result, nil
}

// readForPatch reads the whole file from one descriptor, checks its digest
// and returns the plan that pins the version it read.
func readForPatch(ctx context.Context, path, expected string) (commitPlan, []byte, error) {
	file, info, err := openRegularFile(path)
	if err != nil {
		return commitPlan{}, nil, err
	}
	plan := commitPlan{mode: info.Mode().Perm(), current: file, path: path, revision: fileRevision(info)}
	if info.Size() > protocol.MaxFilePatchSize {
		plan.close()
		return commitPlan{}, nil, fileFailure(agentapi.ResourceLimit, "daemon: %s is larger than the %d byte patch limit", path, protocol.MaxFilePatchSize)
	}
	content, err := readWhole(ctx, file, info.Size())
	if err != nil {
		plan.close()
		return commitPlan{}, nil, err
	}
	if actual := digestHex(content); actual != expected {
		plan.close()
		return commitPlan{}, nil, fileFailure(agentapi.FileConflict, "daemon: %s has sha256 %s, not the expected %s; read it again", path, actual, expected)
	}
	after, err := file.Stat()
	if err != nil || fileRevision(after) != plan.revision {
		plan.close()
		return commitPlan{}, nil, fileFailure(agentapi.FileConflict, "daemon: %s changed while it was read for patching; read it again", path)
	}
	return plan, content, nil
}

func readWhole(ctx context.Context, file *os.File, size int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("daemon: read cancelled: %w", err)
	}
	content := make([]byte, size)
	if n, err := file.ReadAt(content, 0); err != nil && !(errors.Is(err, io.EOF) && int64(n) == size) {
		return nil, fileOSFailure("read", file.Name(), err)
	}
	return content, nil
}

func applyEdits(content []byte, edits []protocol.FileEdit) ([]byte, error) {
	for index, edit := range edits {
		if edit.OldText == "" {
			return nil, fileFailure(agentapi.UsageInvalid, "daemon: edit %d has empty oldText", index)
		}
		old := []byte(edit.OldText)
		switch count := bytes.Count(content, old); count {
		case 1:
			content = bytes.Replace(content, old, []byte(edit.NewText), 1)
		case 0:
			return nil, fileFailure(agentapi.FilePatchMismatch, "daemon: edit %d: oldText does not occur in the file", index)
		default:
			return nil, fileFailure(agentapi.FilePatchMismatch, "daemon: edit %d: oldText occurs %d times; include more context to make it exact", index, count)
		}
		if len(content) > protocol.MaxFilePatchSize {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: patched file would exceed %d bytes", protocol.MaxFilePatchSize)
		}
	}
	return content, nil
}
