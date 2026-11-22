package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
)

// FileExchange sends one control request to one verified host and returns its
// reply. The MCP frontend supplies one over its pooled connection; exchangeOnConn
// wraps a single connection.
type FileExchange func(ctx context.Context, request protocol.Control) (protocol.Control, error)

// HostFiles runs the daemon's file controls against one verified host. The
// exchange's connection proves the host; Target repeats it on every request so
// the daemon can refuse a request meant for another host or OS account, and so
// a result names exactly where it ran. A request names a context in its own
// Target.ContextID, and Caller is then the application credential that owns it.
//
// Paths are absolute, or relative to the named context's working directory,
// which the daemon resolves and authorizes on the owning host.
type HostFiles struct {
	Exchange FileExchange
	Target   protocol.FileTarget
	Caller   protocol.CallerCredential
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

// bind fixes the host and account of a request and keeps its context.
func (h HostFiles) bind(target protocol.FileTarget) protocol.FileTarget {
	return protocol.FileTarget{HostID: h.Target.HostID, Account: h.Target.Account, ContextID: target.ContextID}
}

// Read returns one page of a regular file.
func (h HostFiles) Read(ctx context.Context, request protocol.FileReadRequest) (protocol.FileReadResult, error) {
	request.Target = h.bind(request.Target)
	return fileControl[protocol.FileReadResult](ctx, h, protocol.TypeFileRead, protocol.TypeFileReadResult, request)
}

// List returns one page of a directory.
func (h HostFiles) List(ctx context.Context, request protocol.FileListRequest) (protocol.FileListResult, error) {
	request.Target = h.bind(request.Target)
	return fileControl[protocol.FileListResult](ctx, h, protocol.TypeFileList, protocol.TypeFileListResult, request)
}

// Search looks for a Go regular expression under a directory or in one file.
func (h HostFiles) Search(ctx context.Context, request protocol.FileSearchRequest) (protocol.FileSearchResult, error) {
	request.Target = h.bind(request.Target)
	return fileControl[protocol.FileSearchResult](ctx, h, protocol.TypeFileSearch, protocol.TypeFileSearchResult, request)
}

// Write replaces or creates a file, checking ExpectedSHA256 or CreateOnly at
// the daemon's commit.
func (h HostFiles) Write(ctx context.Context, request protocol.FileWriteRequest) (protocol.FileWriteResult, error) {
	request.Target = h.bind(request.Target)
	return fileControl[protocol.FileWriteResult](ctx, h, protocol.TypeFileWrite, protocol.TypeFileWriteResult, request)
}

// Patch applies exact edits to a file whose SHA-256 matches.
func (h HostFiles) Patch(ctx context.Context, request protocol.FilePatchRequest) (protocol.FilePatchResult, error) {
	request.Target = h.bind(request.Target)
	return fileControl[protocol.FilePatchResult](ctx, h, protocol.TypeFilePatch, protocol.TypeFilePatchResult, request)
}

// fileFrameMargin leaves room for the control envelope around the document.
const fileFrameMargin = 4096

func fileControl[R any](ctx context.Context, files HostFiles, requestType, resultType string, request any) (R, error) {
	var result R
	if files.Exchange == nil {
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
	response, err := files.Exchange(ctx, protocol.Control{Type: requestType, RequestID: id, Caller: files.Caller, Files: document})
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
