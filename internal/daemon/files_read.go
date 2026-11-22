package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

// read returns one page of a regular file from one open descriptor and
// verifies the file's revision after reading, so a page never mixes bytes from
// two versions of a file.
func (c *fileController) read(ctx context.Context, request protocol.FileReadRequest) (protocol.FileReadResult, error) {
	target, path, err := c.beginFileRequest(request.Target, request.Path)
	if err != nil {
		return protocol.FileReadResult{}, err
	}
	offset, wantRevision, err := c.readStart(request, path)
	if err != nil {
		return protocol.FileReadResult{}, err
	}
	if request.MaxBytes < 0 || request.MaxBytes > protocol.MaxFilePage {
		return protocol.FileReadResult{}, fileFailure(agentapi.UsageInvalid, "daemon: maxBytes must be between 0 and %d", protocol.MaxFilePage)
	}
	limit := request.MaxBytes
	if limit == 0 {
		limit = protocol.MaxFilePage
	}
	file, info, err := openRegularFile(path)
	if err != nil {
		return protocol.FileReadResult{}, err
	}
	defer file.Close() //nolint:errcheck // The descriptor is read-only.
	revision := fileRevision(info)
	if wantRevision != "" && wantRevision != revision {
		return protocol.FileReadResult{}, fileFailure(agentapi.FileChanged, "daemon: %s changed since the cursor was issued; read it again from the start", path)
	}
	size := uint64(info.Size()) //nolint:gosec // a file size is never negative
	page, err := readFilePage(file, size, offset, limit)
	if err != nil {
		return protocol.FileReadResult{}, err
	}
	result := protocol.FileReadResult{Target: target, Path: path, Start: offset, Size: size, Revision: revision}
	result.Encoding, result.Data, page = encodePage(page)
	result.End = offset + uint64(len(page))
	result.EOF = result.End >= size
	if result.SHA256, err = readDigest(ctx, file, size, request.Digest); err != nil {
		return protocol.FileReadResult{}, err
	}
	// The final stat is what makes the page a snapshot: a write during the read
	// or the hash changes the fingerprint.
	if err := verifyUnchanged(file, path, revision); err != nil {
		return protocol.FileReadResult{}, err
	}
	if !result.EOF {
		result.NextCursor = c.encodeCursor(fileCursor{Kind: fileCursorRead, Path: path, Offset: result.End, Revision: revision})
	}
	return result, nil
}

// readStart takes the offset from the request or, on a continuation, from the
// verified cursor.
func (c *fileController) readStart(request protocol.FileReadRequest, path string) (uint64, string, error) {
	if request.Cursor == "" {
		return request.Offset, "", nil
	}
	if request.Offset != 0 {
		return 0, "", fileFailure(agentapi.UsageInvalid, "daemon: send either an offset or a cursor, not both")
	}
	cursor, err := c.decodeCursor(request.Cursor, fileCursorRead, path)
	if err != nil {
		return 0, "", err
	}
	return cursor.Offset, cursor.Revision, nil
}

// readDigest hashes small files always and large files only on request.
func readDigest(ctx context.Context, file *os.File, size uint64, wanted bool) (string, error) {
	if size > protocol.MaxFileDigestSize && !wanted {
		return "", nil
	}
	return hashFile(ctx, file, int64(size)) //nolint:gosec // size came from an int64 file size
}

func verifyUnchanged(file *os.File, path, revision string) error {
	after, err := file.Stat()
	if err != nil {
		return fileOSFailure("stat", path, err)
	}
	if fileRevision(after) != revision {
		return fileFailure(agentapi.FileChanged, "daemon: %s changed while it was being read; read it again", path)
	}
	return nil
}

func readFilePage(file readerAt, size, offset uint64, limit int) ([]byte, error) {
	if offset >= size {
		return nil, nil
	}
	length := min(uint64(limit), size-offset) //nolint:gosec // limit is between 1 and MaxFilePage
	page := make([]byte, length)
	n, err := file.ReadAt(page, int64(offset))                          //nolint:gosec // offset is below a file size that is an int64
	if err != nil && !(errors.Is(err, io.EOF) && uint64(n) == length) { //nolint:gosec // n is never negative
		// A short read means the file shrank after the stat.
		return nil, fileFailure(agentapi.FileChanged, "daemon: file shrank while it was being read: %w", err)
	}
	return page, nil
}

type readerAt interface {
	ReadAt([]byte, int64) (int, error)
}

// encodePage returns the page as text when that is exact and fits the encoded
// budget, and as Base64 otherwise. A page that ends inside a character gives
// up the partial character, which the next page starts with; the reported end
// offset accounts for it.
func encodePage(page []byte) (string, string, []byte) {
	if len(page) == 0 {
		return protocol.FileEncodingUTF8, "", page
	}
	text := page
	if !utf8.Valid(text) {
		text = trimPartialRune(text)
	}
	if len(text) > 0 && utf8.Valid(text) && bytes.IndexByte(text, 0) < 0 {
		if escapedLength(text) <= protocol.MaxFileTextEncoded {
			return protocol.FileEncodingUTF8, string(text), text
		}
	}
	return protocol.FileEncodingBase64, base64.StdEncoding.EncodeToString(page), page
}

// trimPartialRune drops an incomplete character from the end of data.
func trimPartialRune(data []byte) []byte {
	for back := 1; back <= utf8.UTFMax-1 && back <= len(data); back++ {
		tail := data[len(data)-back:]
		if utf8.RuneStart(tail[0]) {
			if !utf8.FullRune(tail) {
				return data[:len(data)-back]
			}
			return data
		}
	}
	return data
}

func escapedLength(text []byte) int {
	encoded, err := json.Marshal(string(text))
	if err != nil {
		return len(text) * 6
	}
	return len(encoded) - 2
}
