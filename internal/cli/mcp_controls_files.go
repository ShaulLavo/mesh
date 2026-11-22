package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
)

// fileOperation names the agent operation of one file control, for results and
// errors.
const (
	opFilesRead   = "files.read"
	opFilesList   = "files.list"
	opFilesSearch = "files.search"
	opFilesWrite  = "files.write"
	opFilesPatch  = "files.patch"
	opFilesCopy   = "files.copy"
)

// fileSession is one resolved, verified host a file request runs on.
type fileSession struct {
	host  HostRecord
	info  protocol.HostInfo
	meta  *mcpnative.TargetMeta
	files HostFiles
}

// openFileSession resolves the host (and the context's host), reads its pinned
// identity and capabilities, and builds the exact target the daemon will check:
// host ID, OS account and, when a context is named, its ID. It never wakes a
// host.
func (c *mcpControls) openFileSession(ctx context.Context, operation string, ref mcpnative.FileTargetRef, retry bool) (fileSession, *mcpnative.CallOutcome, error) {
	if ref.Context != nil && c.caller == "" {
		failure := c.identityRequiredOutcome(operation, ref.Context.HostID)
		return fileSession{}, &failure, nil
	}
	host, err := c.resolveFileHost(ctx, ref)
	if err != nil {
		return fileSession{}, nil, err
	}
	meta := c.targetMeta(host, protocol.HostInfo{})
	if ref.Context != nil && host.ID != ref.Context.HostID {
		failure := failOutcome(operation, meta, agentapi.Usage(operation, "The context handle belongs to another host than host.").Err)
		return fileSession{}, &failure, nil
	}
	info, err := c.pool.HostInfo(ctx, host)
	if err != nil {
		failure := failOutcome(operation, meta, agentError(err, operation, host.ID, agentapi.HostUnreachable))
		return fileSession{}, &failure, nil
	}
	meta = c.targetMeta(host, info)
	if err := FileControlsSupported(info); err != nil {
		failure := failOutcome(operation, meta, agentError(err, operation, host.ID, agentapi.ProtocolUnsupported))
		return fileSession{}, &failure, nil
	}
	target := protocol.FileTarget{HostID: host.ID, Account: meta.Account}
	session := fileSession{host: host, info: info, meta: meta}
	session.files = HostFiles{Target: target, Exchange: c.fileExchange(host, retry)}
	if ref.Context != nil {
		session.files.Caller = c.caller
	}
	return session, nil, nil
}

func (c *mcpControls) resolveFileHost(ctx context.Context, ref mcpnative.FileTargetRef) (HostRecord, error) {
	if ref.Host == "" && ref.Context != nil {
		return c.resolveExactHostID(ref.Context.HostID)
	}
	return c.resolveSessionHost(ctx, ref.Host)
}

// fileExchange sends one file control over the host's pooled connection. A
// read may redial once when an idle connection went stale; a write is never
// replayed.
func (c *mcpControls) fileExchange(host HostRecord, retry bool) FileExchange {
	return func(ctx context.Context, request protocol.Control) (protocol.Control, error) {
		response, _, err := c.pool.ExchangeShort(ctx, host, request, retry)
		return response, err
	}
}

// requestContext is the file request's own target: the context ID, if any.
func requestContext(ref mcpnative.FileTargetRef) protocol.FileTarget {
	if ref.Context == nil {
		return protocol.FileTarget{}
	}
	return protocol.FileTarget{ContextID: ref.Context.ContextID}
}

// resultMeta is the target a result reports: where the work ran, down to the
// workspace. The context is shown as the handle the caller used.
func resultMeta(base *mcpnative.TargetMeta, ref mcpnative.FileTargetRef, ran protocol.FileTarget) *mcpnative.TargetMeta {
	meta := *base
	if ref.Context != nil {
		meta.ContextID = ref.Context.String()
		meta.Cwd = ran.Cwd
	}
	if ran.Account != "" {
		meta.Account = ran.Account
	}
	return &meta
}

func (c *mcpControls) FileRead(ctx context.Context, req mcpnative.FileReadRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	session, failed, err := c.openFileSession(ctx, opFilesRead, req.Target, true)
	if failed != nil || err != nil {
		return outcomeOr(failed), err
	}
	result, err := session.files.Read(ctx, protocol.FileReadRequest{Target: requestContext(req.Target), Path: req.Path, Offset: req.Offset, MaxBytes: req.MaxBytes, Cursor: req.Cursor, Digest: req.Digest})
	if err != nil {
		return failOutcome(opFilesRead, session.meta, agentError(err, opFilesRead, req.Path, agentapi.HostUnreachable)), nil
	}
	meta := resultMeta(session.meta, req.Target, result.Target)
	return okOutcome(opFilesRead, meta, mcpnative.FileReadData{
		Target: *meta, Path: result.Path, Encoding: result.Encoding, Data: result.Data,
		Start: strconv.FormatUint(result.Start, 10), End: strconv.FormatUint(result.End, 10), Size: strconv.FormatUint(result.Size, 10),
		EOF: result.EOF, Revision: result.Revision, SHA256: result.SHA256, NextCursor: result.NextCursor,
	}), nil
}

func outcomeOr(failed *mcpnative.CallOutcome) mcpnative.CallOutcome {
	if failed == nil {
		return mcpnative.CallOutcome{}
	}
	return *failed
}

func (c *mcpControls) FileList(ctx context.Context, req mcpnative.FileListRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	session, failed, err := c.openFileSession(ctx, opFilesList, req.Target, true)
	if failed != nil || err != nil {
		return outcomeOr(failed), err
	}
	result, err := session.files.List(ctx, protocol.FileListRequest{Target: requestContext(req.Target), Path: req.Path, Limit: req.Limit, Cursor: req.Cursor})
	if err != nil {
		return failOutcome(opFilesList, session.meta, agentError(err, opFilesList, req.Path, agentapi.HostUnreachable)), nil
	}
	meta := resultMeta(session.meta, req.Target, result.Target)
	entries := make([]mcpnative.FileEntryData, len(result.Entries))
	for i, entry := range result.Entries {
		entries[i] = mcpnative.FileEntryData{Name: entry.Name, Kind: entry.Kind, Mode: entry.Mode, ModTime: entry.ModTime}
		if entry.Kind == protocol.FileKindFile {
			entries[i].Size = strconv.FormatUint(entry.Size, 10)
		}
	}
	return okOutcome(opFilesList, meta, mcpnative.FileListData{Target: *meta, Path: result.Path, Entries: entries, NextCursor: result.NextCursor}), nil
}

func (c *mcpControls) FileSearch(ctx context.Context, req mcpnative.FileSearchRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	session, failed, err := c.openFileSession(ctx, opFilesSearch, req.Target, true)
	if failed != nil || err != nil {
		return outcomeOr(failed), err
	}
	result, err := session.files.Search(ctx, protocol.FileSearchRequest{Target: requestContext(req.Target), Path: req.Path, Pattern: req.Pattern, Glob: req.Glob, MaxMatches: req.MaxMatches})
	if err != nil {
		return failOutcome(opFilesSearch, session.meta, agentError(err, opFilesSearch, req.Path, agentapi.HostUnreachable)), nil
	}
	meta := resultMeta(session.meta, req.Target, result.Target)
	matches := make([]mcpnative.FileMatchData, len(result.Matches))
	for i, match := range result.Matches {
		matches[i] = mcpnative.FileMatchData{Path: match.Path, Line: match.Line, Column: match.Column, Text: match.Text}
	}
	return okOutcome(opFilesSearch, meta, mcpnative.FileSearchData{
		Target: *meta, Path: result.Path, Matches: matches, FilesVisited: result.FilesVisited,
		DirectoriesVisited: result.DirectoriesVisited, Skipped: result.Skipped, Truncated: result.Truncated,
	}), nil
}

func (c *mcpControls) FileWrite(ctx context.Context, req mcpnative.FileWriteRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	session, failed, err := c.openFileSession(ctx, opFilesWrite, req.Target, false)
	if failed != nil || err != nil {
		return outcomeOr(failed), err
	}
	encoding, text := encodeWriteData(req.Data)
	result, err := session.files.Write(ctx, protocol.FileWriteRequest{
		Target: requestContext(req.Target), Path: req.Path, Encoding: encoding, Data: text,
		ExpectedSHA256: req.ExpectedSHA256, CreateOnly: req.CreateOnly,
	})
	if err != nil {
		return failOutcome(opFilesWrite, session.meta, writeFailure(err, opFilesWrite, req.Path)), nil
	}
	meta := resultMeta(session.meta, req.Target, result.Target)
	return okOutcome(opFilesWrite, meta, mcpnative.FileWriteData{
		Target: *meta, Path: result.Path, Size: strconv.FormatUint(result.Size, 10), SHA256: result.SHA256, Revision: result.Revision, Created: result.Created,
	}), nil
}

func (c *mcpControls) FilePatch(ctx context.Context, req mcpnative.FilePatchRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	session, failed, err := c.openFileSession(ctx, opFilesPatch, req.Target, false)
	if failed != nil || err != nil {
		return outcomeOr(failed), err
	}
	edits := make([]protocol.FileEdit, len(req.Edits))
	for i, edit := range req.Edits {
		edits[i] = protocol.FileEdit{OldText: edit.OldText, NewText: edit.NewText}
	}
	result, err := session.files.Patch(ctx, protocol.FilePatchRequest{Target: requestContext(req.Target), Path: req.Path, ExpectedSHA256: req.ExpectedSHA256, Edits: edits})
	if err != nil {
		return failOutcome(opFilesPatch, session.meta, writeFailure(err, opFilesPatch, req.Path)), nil
	}
	meta := resultMeta(session.meta, req.Target, result.Target)
	return okOutcome(opFilesPatch, meta, mcpnative.FileWriteData{
		Target: *meta, Path: result.Path, Size: strconv.FormatUint(result.Size, 10), SHA256: result.SHA256, Revision: result.Revision, Edits: result.Edits,
	}), nil
}

// writeFailure reports a failed write. A write whose request left this process
// but whose reply never came may have committed, so its outcome is unknown and
// the caller must read the file before trying again.
func writeFailure(err error, operation, path string) agentapi.Error {
	var rejected *ControlError
	if errors.As(err, &rejected) || isPreDispatchError(err) {
		return agentError(err, operation, path, agentapi.HostUnreachable)
	}
	return agentapi.Fail(agentapi.RequestOutcomeUnknown, operation, path, fmt.Sprintf("The connection failed after the request was sent, so the write may have been applied: %v. Read the file with mesh_file_read and compare its sha256 before retrying.", err)).Err
}

// encodeWriteData sends text as UTF-8 when its JSON escaping fits the frame
// budget, and anything else as Base64.
func encodeWriteData(data []byte) (string, string) {
	if utf8.Valid(data) {
		if escaped, err := json.Marshal(string(data)); err == nil && len(escaped)-2 <= protocol.MaxFileTextEncoded {
			return protocol.FileEncodingUTF8, string(data)
		}
	}
	return protocol.FileEncodingBase64, base64.StdEncoding.EncodeToString(data)
}

// Copy moves a file or directory between this machine and one remote host
// over a dedicated authenticated connection, because a transfer keeps state on
// its connection.
func (c *mcpControls) Copy(ctx context.Context, req mcpnative.CopyRequest) (mcpnative.CallOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	host, err := c.resolveSessionHost(ctx, req.Host)
	if err != nil {
		return mcpnative.CallOutcome{}, err
	}
	meta := c.targetMeta(host, protocol.HostInfo{})
	if host.local {
		return failOutcome(opFilesCopy, meta, agentapi.Usage(opFilesCopy, "mesh_copy moves files between this machine and a remote host. This host is the machine itself; use the file tools or ordinary tools here.").Err), nil
	}
	conn, info, err := openVerifiedHostInfo(ctx, host, c.app.dependencies.DialControl)
	if err != nil {
		return failOutcome(opFilesCopy, meta, agentError(err, opFilesCopy, host.ID, agentapi.HostUnreachable)), nil
	}
	defer conn.Close() //nolint:errcheck // The transfer result decides success.
	meta = c.targetMeta(host, info)
	var transferred int64
	if req.Direction == mcpnative.CopyUpload {
		transferred, err = uploadCopy(ctx, conn, req.LocalPath, req.RemotePath, req.Recursive)
	} else {
		transferred, err = downloadCopy(ctx, conn, req.RemotePath, req.LocalPath, req.Recursive)
	}
	if err != nil {
		return failOutcome(opFilesCopy, meta, agentError(err, opFilesCopy, req.RemotePath, agentapi.HostUnreachable)), nil
	}
	return okOutcome(opFilesCopy, meta, mcpnative.CopyData{
		Target: *meta, Direction: req.Direction, LocalPath: req.LocalPath, RemotePath: req.RemotePath,
		Recursive: req.Recursive, Bytes: strconv.FormatInt(transferred, 10),
	}), nil
}
