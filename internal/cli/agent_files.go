package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// HostFiles runs the daemon's file controls on one verified connection to one
// host. The connection's pin proves the host; Target repeats it on every
// request so the daemon can refuse a request meant for another host or OS
// account, and so a result names exactly where it ran.
//
// Paths are absolute today. When contexts land, a relative path will resolve on
// the daemon against an authorized context's working directory, which needs a
// new request field and nothing in this type.
type HostFiles struct {
	Conn   transport.Conn
	Target protocol.FileTarget
}

// FileControlsSupported reports whether the host's daemon serves file
// controls, using the host.info it already read. A host without them is a
// protocol.unsupported failure before any file request is sent.
func FileControlsSupported(info protocol.HostInfo) error {
	if info.FilesSupported {
		return nil
	}
	return typedControlError("control.unknown", "file controls", "", "cli: this host's daemon does not serve file controls; update it")
}

// Read returns one page of a regular file.
func (h HostFiles) Read(ctx context.Context, request protocol.FileReadRequest) (protocol.FileReadResult, error) {
	request.Target = h.Target
	return fileControl[protocol.FileReadResult](ctx, h.Conn, protocol.TypeFileRead, protocol.TypeFileReadResult, request)
}

// List returns one page of a directory.
func (h HostFiles) List(ctx context.Context, request protocol.FileListRequest) (protocol.FileListResult, error) {
	request.Target = h.Target
	return fileControl[protocol.FileListResult](ctx, h.Conn, protocol.TypeFileList, protocol.TypeFileListResult, request)
}

// Search looks for a Go regular expression under a directory or in one file.
func (h HostFiles) Search(ctx context.Context, request protocol.FileSearchRequest) (protocol.FileSearchResult, error) {
	request.Target = h.Target
	return fileControl[protocol.FileSearchResult](ctx, h.Conn, protocol.TypeFileSearch, protocol.TypeFileSearchResult, request)
}

// Write replaces or creates a file, checking ExpectedSHA256 or CreateOnly at
// the daemon's commit.
func (h HostFiles) Write(ctx context.Context, request protocol.FileWriteRequest) (protocol.FileWriteResult, error) {
	request.Target = h.Target
	return fileControl[protocol.FileWriteResult](ctx, h.Conn, protocol.TypeFileWrite, protocol.TypeFileWriteResult, request)
}

// Patch applies exact edits to a file whose SHA-256 matches.
func (h HostFiles) Patch(ctx context.Context, request protocol.FilePatchRequest) (protocol.FilePatchResult, error) {
	request.Target = h.Target
	return fileControl[protocol.FilePatchResult](ctx, h.Conn, protocol.TypeFilePatch, protocol.TypeFilePatchResult, request)
}

// fileFrameMargin leaves room for the control envelope around the document.
const fileFrameMargin = 4096

func fileControl[R any](ctx context.Context, conn transport.Conn, requestType, resultType string, request any) (R, error) {
	var result R
	if conn == nil {
		return result, errors.New("cli: file request without a connection")
	}
	document, err := protocol.EncodeFileDocument(request)
	if err != nil {
		return result, fmt.Errorf("cli: %s: %w", requestType, err)
	}
	if len(document) > protocol.MaxPayload-fileFrameMargin {
		return result, typedControlError("resource.limit", requestType, "", fmt.Sprintf("cli: %s request does not fit one frame; send less data", requestType))
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return result, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: requestType, RequestID: id, Files: document})
	if err != nil {
		return result, err
	}
	if response.Type == protocol.TypeError {
		return result, daemonResponseError(requestType, response.Message, response.ErrorCode)
	}
	if response.Type != resultType {
		return result, fmt.Errorf("cli: host answered %s with %s", requestType, response.Type)
	}
	if err := protocol.DecodeFileDocument(response.Files, &result); err != nil {
		return result, fmt.Errorf("cli: %s result: %w", requestType, err)
	}
	return result, nil
}
