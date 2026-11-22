package mcpnative

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
)

const (
	maxFileCursorLength = 4096
	maxProfileDocument  = 32 << 10
	defaultCopyMS       = 120000
	maxCopyMS           = 600000
)

// fileTargetArgs reads the host, context and path every file tool takes.
func fileTarget(host, contextHandle, path string) (FileTargetRef, string, error) {
	var target FileTargetRef
	target.Host = strings.TrimSpace(host)
	if contextHandle != "" {
		ref, err := ParseContextHandle(contextHandle)
		if err != nil {
			return target, "", invalidParam("context: %v", err)
		}
		target.Context = &ref
	}
	if target.Host == "" && target.Context == nil {
		return target, "", invalidParam("host is required (or a context handle that names it)")
	}
	if path == "" || len(path) > protocol.MaxFilePath || strings.ContainsRune(path, 0) {
		return target, "", invalidParam("path must be 1 to %d bytes without NUL", protocol.MaxFilePath)
	}
	if target.Context == nil && !strings.HasPrefix(path, "/") {
		return target, "", invalidParam("path must be absolute, or relative with a context handle")
	}
	return target, path, nil
}

func parseDecimalOffset(text string) (uint64, error) {
	if text == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != text {
		return 0, invalidParam("offset must be a nonnegative decimal string")
	}
	return value, nil
}

func validDigest(value string) error {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 || strings.ToLower(value) != value {
		return invalidParam("expected_sha256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func handleFileRead(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		Context   string `json:"context"`
		Path      string `json:"path"`
		Offset    string `json:"offset"`
		MaxBytes  *int   `json:"max_bytes"`
		Cursor    string `json:"cursor"`
		Digest    bool   `json:"digest"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	target, path, err := fileTarget(args.Host, args.Context, args.Path)
	if err != nil {
		return CallOutcome{}, err
	}
	offset, err := parseDecimalOffset(args.Offset)
	if err != nil {
		return CallOutcome{}, err
	}
	if args.Cursor != "" && args.Offset != "" {
		return CallOutcome{}, invalidParam("send either offset or cursor, not both")
	}
	if len(args.Cursor) > maxFileCursorLength {
		return CallOutcome{}, invalidParam("cursor exceeds %d bytes", maxFileCursorLength)
	}
	maxBytes := 0
	if args.MaxBytes != nil {
		if *args.MaxBytes < 1 || *args.MaxBytes > protocol.MaxFilePage {
			return CallOutcome{}, invalidParam("max_bytes must be between 1 and %d", protocol.MaxFilePage)
		}
		maxBytes = *args.MaxBytes
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.FileRead(ctx, FileReadRequest{Target: target, Path: path, Offset: offset, MaxBytes: maxBytes, Cursor: args.Cursor, Digest: args.Digest, Timeout: timeout})
	return wrapControl("mesh_file_read", out, err)
}

func handleFileList(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host      string `json:"host"`
		Context   string `json:"context"`
		Path      string `json:"path"`
		Limit     *int   `json:"limit"`
		Cursor    string `json:"cursor"`
		TimeoutMS *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	target, path, err := fileTarget(args.Host, args.Context, args.Path)
	if err != nil {
		return CallOutcome{}, err
	}
	limit := 0
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > protocol.MaxFileList {
			return CallOutcome{}, invalidParam("limit must be between 1 and %d", protocol.MaxFileList)
		}
		limit = *args.Limit
	}
	if len(args.Cursor) > maxFileCursorLength {
		return CallOutcome{}, invalidParam("cursor exceeds %d bytes", maxFileCursorLength)
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.FileList(ctx, FileListRequest{Target: target, Path: path, Limit: limit, Cursor: args.Cursor, Timeout: timeout})
	return wrapControl("mesh_file_list", out, err)
}

func handleFileSearch(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host       string `json:"host"`
		Context    string `json:"context"`
		Path       string `json:"path"`
		Pattern    string `json:"pattern"`
		Glob       string `json:"glob"`
		MaxMatches *int   `json:"max_matches"`
		TimeoutMS  *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	target, path, err := fileTarget(args.Host, args.Context, args.Path)
	if err != nil {
		return CallOutcome{}, err
	}
	if args.Pattern == "" || len(args.Pattern) > protocol.MaxFilePattern || len(args.Glob) > protocol.MaxFilePattern {
		return CallOutcome{}, invalidParam("pattern must be 1 to %d bytes", protocol.MaxFilePattern)
	}
	maxMatches := 0
	if args.MaxMatches != nil {
		if *args.MaxMatches < 1 || *args.MaxMatches > protocol.MaxFileSearchMatches {
			return CallOutcome{}, invalidParam("max_matches must be between 1 and %d", protocol.MaxFileSearchMatches)
		}
		maxMatches = *args.MaxMatches
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.FileSearch(ctx, FileSearchRequest{Target: target, Path: path, Pattern: args.Pattern, Glob: args.Glob, MaxMatches: maxMatches, Timeout: timeout})
	return wrapControl("mesh_file_search", out, err)
}

func handleFileWrite(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host           string  `json:"host"`
		Context        string  `json:"context"`
		Path           string  `json:"path"`
		Content        *string `json:"content"`
		ContentBase64  *string `json:"content_base64"`
		ExpectedSHA256 string  `json:"expected_sha256"`
		CreateOnly     bool    `json:"create_only"`
		TimeoutMS      *int    `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	target, path, err := fileTarget(args.Host, args.Context, args.Path)
	if err != nil {
		return CallOutcome{}, err
	}
	data, err := writeContent(args.Content, args.ContentBase64)
	if err != nil {
		return CallOutcome{}, err
	}
	if args.CreateOnly == (args.ExpectedSHA256 != "") {
		return CallOutcome{}, invalidParam("give exactly one of expected_sha256 and create_only")
	}
	if args.ExpectedSHA256 != "" {
		if err := validDigest(args.ExpectedSHA256); err != nil {
			return CallOutcome{}, err
		}
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.FileWrite(ctx, FileWriteRequest{Target: target, Path: path, Data: data, ExpectedSHA256: args.ExpectedSHA256, CreateOnly: args.CreateOnly, Timeout: timeout})
	return wrapControl("mesh_file_write", out, err)
}

// writeContent takes exactly one of text and Base64 content and enforces the
// 1 MiB raw bound before anything is sent.
func writeContent(text, encoded *string) ([]byte, error) {
	if (text == nil) == (encoded == nil) {
		return nil, invalidParam("give exactly one of content and content_base64")
	}
	if text != nil {
		if len(*text) > protocol.MaxFileWrite {
			return nil, invalidParam("content exceeds %d bytes; use mesh_copy for larger files", protocol.MaxFileWrite)
		}
		return []byte(*text), nil
	}
	if base64.StdEncoding.DecodedLen(len(*encoded)) > protocol.MaxFileWrite+3 {
		return nil, invalidParam("content_base64 exceeds %d bytes; use mesh_copy for larger files", protocol.MaxFileWrite)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(*encoded)
	if err != nil {
		return nil, invalidParam("content_base64 is not valid standard base64")
	}
	if len(data) > protocol.MaxFileWrite {
		return nil, invalidParam("content_base64 exceeds %d bytes; use mesh_copy for larger files", protocol.MaxFileWrite)
	}
	return data, nil
}

func handleFilePatch(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host           string `json:"host"`
		Context        string `json:"context"`
		Path           string `json:"path"`
		ExpectedSHA256 string `json:"expected_sha256"`
		Edits          []struct {
			OldText string `json:"old_text"`
			NewText string `json:"new_text"`
		} `json:"edits"`
		TimeoutMS *int `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	target, path, err := fileTarget(args.Host, args.Context, args.Path)
	if err != nil {
		return CallOutcome{}, err
	}
	if err := validDigest(args.ExpectedSHA256); err != nil {
		return CallOutcome{}, err
	}
	if len(args.Edits) == 0 || len(args.Edits) > protocol.MaxFilePatchEdits {
		return CallOutcome{}, invalidParam("edits must hold 1 to %d items", protocol.MaxFilePatchEdits)
	}
	edits := make([]FileEditRequest, len(args.Edits))
	for i, edit := range args.Edits {
		if edit.OldText == "" {
			return CallOutcome{}, invalidParam("edits[%d].old_text must not be empty", i)
		}
		edits[i] = FileEditRequest{OldText: edit.OldText, NewText: edit.NewText}
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultOpMS, maxTimeoutMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.FilePatch(ctx, FilePatchRequest{Target: target, Path: path, ExpectedSHA256: args.ExpectedSHA256, Edits: edits, Timeout: timeout})
	return wrapControl("mesh_file_patch", out, err)
}

func handleCopy(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Host       string `json:"host"`
		Direction  string `json:"direction"`
		LocalPath  string `json:"local_path"`
		RemotePath string `json:"remote_path"`
		Recursive  bool   `json:"recursive"`
		TimeoutMS  *int   `json:"timeout_ms"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	host, err := requireNonEmpty(args.Host, "host")
	if err != nil {
		return CallOutcome{}, err
	}
	if args.Direction != CopyUpload && args.Direction != CopyDownload {
		return CallOutcome{}, invalidParam("direction must be %q or %q", CopyUpload, CopyDownload)
	}
	for name, value := range map[string]string{"local_path": args.LocalPath, "remote_path": args.RemotePath} {
		if !strings.HasPrefix(value, "/") || len(value) > protocol.MaxFilePath || strings.ContainsRune(value, 0) {
			return CallOutcome{}, invalidParam("%s must be an absolute path of at most %d bytes", name, protocol.MaxFilePath)
		}
	}
	timeout, err := parseTimeout(args.TimeoutMS, defaultCopyMS, maxCopyMS)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.Copy(ctx, CopyRequest{Host: host, Direction: args.Direction, LocalPath: args.LocalPath, RemotePath: args.RemotePath, Recursive: args.Recursive, Timeout: timeout})
	return wrapControl("mesh_copy", out, err)
}

func handleFleetStatus(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.FleetStatus(ctx, FleetStatusRequest{})
	return wrapControl("mesh_fleet_status", out, err)
}

func parseProfileScope(scope string) (string, error) {
	switch scope {
	case "", ProfileScopeHost:
		return ProfileScopeHost, nil
	case ProfileScopeFleet:
		return ProfileScopeFleet, nil
	default:
		return "", invalidParam("scope must be %q or %q", ProfileScopeHost, ProfileScopeFleet)
	}
}

func handleProfileGet(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Scope string `json:"scope"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	scope, err := parseProfileScope(args.Scope)
	if err != nil {
		return CallOutcome{}, err
	}
	out, err := controls.ProfileGet(ctx, ProfileGetRequest{Scope: scope})
	return wrapControl("mesh_profile_get", out, err)
}

func handleProfileSet(ctx context.Context, controls Controls, raw json.RawMessage, _ func(ProgressUpdate)) (CallOutcome, error) {
	if err := requireControls(controls); err != nil {
		return CallOutcome{}, err
	}
	var args struct {
		Scope            string          `json:"scope"`
		ExpectedRevision *uint64         `json:"expected_revision"`
		Profile          json.RawMessage `json:"profile"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return CallOutcome{}, err
	}
	scope, err := parseProfileScope(args.Scope)
	if err != nil {
		return CallOutcome{}, err
	}
	if args.ExpectedRevision == nil {
		return CallOutcome{}, invalidParam("expected_revision is required; read it with mesh_profile_get (0 for an absent profile)")
	}
	document := strings.TrimSpace(string(args.Profile))
	if document == "" || document == "null" || !strings.HasPrefix(document, "{") {
		return CallOutcome{}, invalidParam("profile must be an object with schemaVersion, description, labels, warnings and instructions")
	}
	limit := hostprofile.HostLimits.Encoded
	if scope == ProfileScopeFleet {
		limit = hostprofile.FleetLimits.Encoded
	}
	if len(document) > limit {
		return CallOutcome{}, invalidParam("profile exceeds %d encoded bytes for %s notes", limit, scope)
	}
	out, err := controls.ProfileSet(ctx, ProfileSetRequest{Scope: scope, ExpectedRevision: *args.ExpectedRevision, Document: []byte(document)})
	return wrapControl("mesh_profile_set", out, err)
}
