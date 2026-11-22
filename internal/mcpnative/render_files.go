package mcpnative

import (
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
)

func init() {
	for code, hint := range map[agentapi.Code]string{
		agentapi.FileNotFound:         "Check the path with mesh_file_list; a relative path resolves against the context's working directory.",
		agentapi.FileNotRegular:       "Name the real path: symlinks, devices, sockets and pipes are refused.",
		agentapi.FileChanged:          "The file changed since the cursor was issued; read it again from the start.",
		agentapi.FileCursorInvalid:    "Use the nextCursor returned by mesh_file_read or mesh_file_list for this same host and path.",
		agentapi.FileConflict:         "Read the file again with mesh_file_read for its current sha256 and retry; nothing was changed.",
		agentapi.FilePatchMismatch:    "Make old_text match exactly one place, with more surrounding context, and retry; nothing was changed.",
		agentapi.FilePermissionDenied: "The daemon account may not do this; pick a path it can reach.",
		agentapi.FileTargetMismatch:   "The host runs as another account than the one named; re-read the target from mesh_host_inspect.",
		agentapi.ContextNotFound:      "Open a context with mesh_context_open on this host, or pass an absolute path.",
		agentapi.ContextClosed:        "Open a new context, or pass an absolute path.",
		agentapi.ProfileConflict:      "Read the current revision with mesh_profile_get and send the edit again.",
		agentapi.ProfileInvalid:       "Fix the profile document; see the mesh_profile_set schema for the allowed fields and limits.",
	} {
		nextActionHints[code] = hint
	}
}

// renderFileData renders the M6 data types, and nothing else.
func renderFileData(b *strings.Builder, data any) {
	switch v := data.(type) {
	case FileReadData:
		renderFileRead(b, v)
	case FileListData:
		renderFileList(b, v)
	case FileSearchData:
		renderFileSearch(b, v)
	case FileWriteData:
		renderFileWrite(b, v)
	case CopyData:
		fmt.Fprintf(b, "copy %s: %s <-> %s, %s bytes", v.Direction, escapeControlText(v.LocalPath), escapeControlText(v.RemotePath), v.Bytes)
	case agentapi.FleetStatus:
		renderFleetStatus(b, v)
	case agentapi.FleetSection:
		fmt.Fprintf(b, "fleet notes: %s", v.State)
		renderProfileBody(b, v.ProfileSection)
	case agentapi.ProfileSection:
		fmt.Fprintf(b, "host notes: %s", v.State)
		renderProfileBody(b, v)
	case agentapi.ProfileWrite:
		fmt.Fprintf(b, "profile revision %d committed=%t changed=%t publication=%s", v.Revision, v.Committed, v.Changed, v.Publication)
	}
}

func renderFileRead(b *strings.Builder, data FileReadData) {
	fmt.Fprintf(b, "%s [%s..%s of %s] eof=%t encoding=%s revision=%s", escapeControlText(data.Path), data.Start, data.End, data.Size, data.EOF, data.Encoding, data.Revision)
	if data.SHA256 != "" {
		fmt.Fprintf(b, " sha256=%s", data.SHA256)
	}
	if data.Data == "" {
		return
	}
	b.WriteByte('\n')
	if data.Encoding == protocol.FileEncodingUTF8 {
		b.WriteString(escapeControlText(data.Data))
		return
	}
	b.WriteString(data.Data)
}

func renderFileList(b *strings.Builder, data FileListData) {
	fmt.Fprintf(b, "%s: %d entries", escapeControlText(data.Path), len(data.Entries))
	for _, entry := range data.Entries {
		fmt.Fprintf(b, "\n%s %s", entry.Kind, escapeControlText(entry.Name))
	}
}

func renderFileSearch(b *strings.Builder, data FileSearchData) {
	fmt.Fprintf(b, "%s: %d matches in %d files", escapeControlText(data.Path), len(data.Matches), data.FilesVisited)
	if data.Truncated != "" {
		fmt.Fprintf(b, " (stopped at %s)", data.Truncated)
	}
	for _, match := range data.Matches {
		fmt.Fprintf(b, "\n%s:%d:%d: %s", escapeControlText(match.Path), match.Line, match.Column, escapeControlText(match.Text))
	}
}

func renderFileWrite(b *strings.Builder, data FileWriteData) {
	verb := "wrote"
	if data.Edits > 0 {
		verb = fmt.Sprintf("patched (%d edits)", data.Edits)
	}
	fmt.Fprintf(b, "%s %s: %s bytes sha256=%s revision=%s", verb, escapeControlText(data.Path), data.Size, data.SHA256, data.Revision)
}

func renderFleetStatus(b *strings.Builder, status agentapi.FleetStatus) {
	fmt.Fprintf(b, "fleet %q saved=%t replica=%s", escapeControlText(status.Fleet), status.FleetSaved, status.Replica.State)
	for _, member := range status.Members {
		fmt.Fprintf(b, "\n%s %s", member.HostID, member.State)
	}
}

func renderProfileBody(b *strings.Builder, section agentapi.ProfileSection) {
	if section.Revision != nil {
		fmt.Fprintf(b, " revision=%d", *section.Revision)
	}
	if section.Profile == nil {
		return
	}
	if section.Profile.Description != "" {
		fmt.Fprintf(b, "\n%s", escapeControlText(section.Profile.Description))
	}
	if section.Profile.Instructions != nil && *section.Profile.Instructions != "" {
		fmt.Fprintf(b, "\n%s", escapeControlText(*section.Profile.Instructions))
	}
}
