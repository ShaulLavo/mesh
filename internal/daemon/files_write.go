package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// newFilePermission is the mode of a file fs.write creates. An existing file
// keeps its own mode bits, including setuid, setgid and sticky; its owner and
// group become the daemon account's, because the replacement is a new file.
const newFilePermission uint32 = 0o644

// write replaces or creates one regular file through a temporary file in the
// same directory and a rename, so a reader sees the old or the new bytes and
// never a mix. The caller's precondition is checked at commit.
func (c *fileController) write(ctx context.Context, request protocol.FileWriteRequest) (protocol.FileWriteResult, error) {
	target, path, err := c.beginFileRequest(ctx, request.Target, request.Path)
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
	release, err := c.acquireWrite(ctx)
	if err != nil {
		return protocol.FileWriteResult{}, err
	}
	defer release()
	location, err := locateFile(path)
	if err != nil {
		return protocol.FileWriteResult{}, err
	}
	defer location.close()
	plan := commitPlan{permission: newFilePermission, noReplace: true}
	if !request.CreateOnly {
		if plan, err = replacePlan(ctx, location, expected); err != nil {
			return protocol.FileWriteResult{}, err
		}
	}
	defer plan.close()
	installed, err := c.commit(ctx, location, data, plan)
	if err != nil {
		return protocol.FileWriteResult{}, err
	}
	return protocol.FileWriteResult{Target: target, Path: path, Size: uint64(len(data)), SHA256: digestHex(data), Revision: installed.revision(), Created: request.CreateOnly}, nil
}

// acquireWrite waits for this daemon's commit slot until the caller goes away.
func (c *fileController) acquireWrite(ctx context.Context) (func(), error) {
	select {
	case c.writeSlot <- struct{}{}:
		return func() { <-c.writeSlot }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("daemon: write cancelled while waiting to commit: %w", ctx.Err())
	}
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

// commitPlan is what a commit protects. A replacement holds the open
// descriptor and stat of the version whose digest matched.
type commitPlan struct {
	permission uint32
	noReplace  bool
	current    *os.File
	hashed     fileStat
}

func (p commitPlan) close() {
	if p.current != nil {
		_ = p.current.Close()
	}
}

// replacePlan verifies the digest of the file being replaced, from one open
// descriptor, and records the stat of the version it hashed.
func replacePlan(ctx context.Context, location fileLocation, expected string) (commitPlan, error) {
	file, before, err := location.openRegular()
	if err != nil {
		return commitPlan{}, err
	}
	actual, err := hashFile(ctx, file, int64(before.size)) //nolint:gosec // size came from an int64 file size
	if err != nil {
		_ = file.Close()
		return commitPlan{}, err
	}
	if actual != expected {
		_ = file.Close()
		return commitPlan{}, fileFailure(agentapi.FileConflict, "daemon: %s has sha256 %s, not the expected %s; read it again", location.path, actual, expected)
	}
	after, err := fstatFile(file)
	if err != nil || after.revision() != before.revision() {
		_ = file.Close()
		return commitPlan{}, fileFailure(agentapi.FileConflict, "daemon: %s changed while its digest was checked; read it again", location.path)
	}
	return commitPlan{permission: after.permission(), current: file, hashed: after}, nil
}

// commit stages the new contents beside the target and installs them. The
// request must still be live, and its device still approved, immediately
// before the install. The result is the stat of the descriptor that was
// installed, so it describes this write and never a later writer's file.
func (c *fileController) commit(ctx context.Context, location fileLocation, data []byte, plan commitPlan) (fileStat, error) {
	staged, err := createStaged(location.dir, location.path)
	if err != nil {
		return fileStat{}, err
	}
	keep := false
	defer func() {
		if !keep {
			staged.discard()
		}
	}()
	if err := staged.fill(data, plan.permission); err != nil {
		return fileStat{}, fileOSFailure("write", location.path, err)
	}
	c.stage("before-commit", location.path)
	if err := c.admit(ctx); err != nil {
		return fileStat{}, err
	}
	c.stage("installing", location.path)
	if plan.noReplace {
		err = installNew(location, staged)
	} else {
		err = c.installReplacement(location, staged, plan, &keep)
	}
	if err != nil {
		return fileStat{}, err
	}
	keep = true
	c.stage("committed", location.path)
	defer staged.file.Close() //nolint:errcheck // The descriptor was only written before the install.
	syncDirectory(location.dir)
	installed, err := fstatFile(staged.file)
	if err != nil {
		return fileStat{}, fileFailure(agentapi.InternalError, "daemon: %s was written but its revision could not be read: %w", location.path, err)
	}
	return installed, nil
}

// installNew creates the target name atomically, failing when it exists.
func installNew(location fileLocation, staged stagedFile) error {
	fd := int(location.dir.Fd())
	err := renameNoReplaceAt(fd, staged.name, location.name)
	if isNotSupported(err) {
		// A filesystem without no-replace rename still has an atomic exclusive
		// hard link.
		if err = unix.Linkat(fd, staged.name, fd, location.name, 0); err == nil {
			_ = unix.Unlinkat(fd, staged.name, 0)
		}
	}
	if errors.Is(err, unix.EEXIST) {
		return fileFailure(agentapi.FileConflict, "daemon: %s already exists", location.path)
	}
	if err != nil {
		return fileOSFailure("create", location.path, err)
	}
	return nil
}

// installReplacement swaps the staged file with the target in one atomic
// step, then checks that the file it displaced is the version whose digest
// matched. A writer that saved between that check and the swap is found here,
// and its version is put back, so the caller's precondition holds even
// against another process. A filesystem that cannot swap falls back to a
// check and a rename, which a save in the gap between them can still beat.
func (c *fileController) installReplacement(location fileLocation, staged stagedFile, plan commitPlan, keep *bool) error {
	fd := int(location.dir.Fd())
	err := exchangeAt(fd, staged.name, location.name)
	if isNotSupported(err) {
		return replaceByRename(location, staged, plan)
	}
	if errors.Is(err, unix.ENOENT) {
		return fileFailure(agentapi.FileConflict, "daemon: %s was removed before the write committed", location.path)
	}
	if err != nil {
		return fileOSFailure("replace", location.path, err)
	}
	c.stage("exchanged", location.path)
	displaced, statErr := statAt(location.dir, staged.name)
	if statErr == nil && sameVersion(displaced, plan.hashed) && descriptorUnchanged(plan) {
		_ = unix.Unlinkat(fd, staged.name, 0)
		return nil
	}
	return undoReplacement(location, staged, keep)
}

// sameVersion reports whether a stat is the version that was hashed.
func sameVersion(found, hashed fileStat) bool {
	return found.sameFile(hashed) && found.size == hashed.size && found.mtime == hashed.mtime
}

// descriptorUnchanged checks the held descriptor, which follows the inode
// whatever name it has now, so an in-place write to it is seen too.
func descriptorUnchanged(plan commitPlan) bool {
	now, err := fstatFile(plan.current)
	return err == nil && sameVersion(now, plan.hashed)
}

// undoReplacement runs after a swap displaced a version the caller never saw.
// It swaps back so that version stays where it was and the write is refused.
func undoReplacement(location fileLocation, staged stagedFile, keep *bool) error {
	fd := int(location.dir.Fd())
	mine, mineErr := fstatFile(staged.file)
	current, currentErr := statAt(location.dir, location.name)
	if mineErr == nil && currentErr == nil && current.sameFile(mine) {
		if err := exchangeAt(fd, staged.name, location.name); err == nil {
			return fileFailure(agentapi.FileConflict, "daemon: %s changed while the write committed; it was left as the other writer saved it, read it again", location.path)
		}
	}
	// A third writer replaced the file in the meantime, or the swap back failed.
	// The newer version the swap displaced is still at the temporary name; keep
	// it rather than delete it.
	*keep = true
	_ = staged.file.Close()
	return fileFailure(agentapi.FileConflict, "daemon: %s changed while the write committed and could not be restored; the version this write displaced is saved as %s in the same directory", location.path, staged.name)
}

// replaceByRename is the fallback for filesystems without an atomic swap.
func replaceByRename(location fileLocation, staged stagedFile, plan commitPlan) error {
	found, err := statAt(location.dir, location.name)
	if err != nil {
		return fileFailure(agentapi.FileConflict, "daemon: %s was removed before the write committed", location.path)
	}
	if !sameVersion(found, plan.hashed) || !descriptorUnchanged(plan) {
		return fileFailure(agentapi.FileConflict, "daemon: %s changed before the write committed; read it again", location.path)
	}
	fd := int(location.dir.Fd())
	if err := unix.Renameat(fd, staged.name, fd, location.name); err != nil {
		return fileOSFailure("rename into", location.path, err)
	}
	return nil
}

// patch applies exact text edits to a file whose digest matches. Each OldText
// must occur exactly once in the text as the earlier edits left it, so an
// ambiguous or stale edit fails instead of landing in the wrong place.
func (c *fileController) patch(ctx context.Context, request protocol.FilePatchRequest) (protocol.FilePatchResult, error) {
	target, path, err := c.beginFileRequest(ctx, request.Target, request.Path)
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
	release, err := c.acquireWrite(ctx)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	defer release()
	location, err := locateFile(path)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	defer location.close()
	plan, content, err := readForPatch(ctx, location, expected)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	defer plan.close()
	patched, err := applyEdits(content, request.Edits)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	installed, err := c.commit(ctx, location, patched, plan)
	if err != nil {
		return protocol.FilePatchResult{}, err
	}
	return protocol.FilePatchResult{Target: target, Path: path, Size: uint64(len(patched)), SHA256: digestHex(patched), Revision: installed.revision(), Edits: len(request.Edits)}, nil
}

// readForPatch reads the whole file from one descriptor, checks its digest
// and returns the plan that pins the version it read.
func readForPatch(ctx context.Context, location fileLocation, expected string) (commitPlan, []byte, error) {
	file, before, err := location.openRegular()
	if err != nil {
		return commitPlan{}, nil, err
	}
	if before.size > protocol.MaxFilePatchSize {
		_ = file.Close()
		return commitPlan{}, nil, fileFailure(agentapi.ResourceLimit, "daemon: %s is larger than the %d byte patch limit", location.path, protocol.MaxFilePatchSize)
	}
	content, err := readWhole(ctx, file, int64(before.size)) //nolint:gosec // bounded by MaxFilePatchSize above
	if err != nil {
		_ = file.Close()
		return commitPlan{}, nil, err
	}
	if actual := digestHex(content); actual != expected {
		_ = file.Close()
		return commitPlan{}, nil, fileFailure(agentapi.FileConflict, "daemon: %s has sha256 %s, not the expected %s; read it again", location.path, actual, expected)
	}
	after, err := fstatFile(file)
	if err != nil || after.revision() != before.revision() {
		_ = file.Close()
		return commitPlan{}, nil, fileFailure(agentapi.FileConflict, "daemon: %s changed while it was read for patching; read it again", location.path)
	}
	return commitPlan{permission: after.permission(), current: file, hashed: after}, content, nil
}

func readWhole(ctx context.Context, file *os.File, size int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("daemon: read cancelled: %w", err)
	}
	content := make([]byte, size)
	if n, err := file.ReadAt(content, 0); err != nil && int64(n) != size {
		return nil, fileOSFailure("read", file.Name(), err)
	}
	return content, nil
}

// applyEdits applies the edits in order. An edit's text must match exactly one
// place, and two matches that overlap are two matches.
func applyEdits(content []byte, edits []protocol.FileEdit) ([]byte, error) {
	for index, edit := range edits {
		if edit.OldText == "" {
			return nil, fileFailure(agentapi.UsageInvalid, "daemon: edit %d has empty oldText", index)
		}
		old := []byte(edit.OldText)
		first := bytes.Index(content, old)
		if first < 0 {
			return nil, fileFailure(agentapi.FilePatchMismatch, "daemon: edit %d: oldText does not occur in the file", index)
		}
		if bytes.Contains(content[first+1:], old) {
			return nil, fileFailure(agentapi.FilePatchMismatch, "daemon: edit %d: oldText occurs more than once; include more context to make it exact", index)
		}
		patched := make([]byte, 0, len(content)-len(old)+len(edit.NewText))
		patched = append(patched, content[:first]...)
		patched = append(patched, edit.NewText...)
		content = append(patched, content[first+len(old):]...)
		if len(content) > protocol.MaxFilePatchSize {
			return nil, fileFailure(agentapi.ResourceLimit, "daemon: patched file would exceed %d bytes", protocol.MaxFilePatchSize)
		}
	}
	return content, nil
}
