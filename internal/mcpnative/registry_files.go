package mcpnative

import "encoding/json"

const (
	groupFiles   = "files"
	groupFleet   = "fleet"
	groupProfile = "profile"
)

const (
	fileHostProp    = `"host":{"type":"string","description":"Machine name or exact host ID. Optional when context names the host."}`
	fileContextProp = `"context":{"type":"string","description":"Opaque context handle (mc1.<hostId>.<contextId>) whose working directory resolves a relative path. Needs an application identity; the owning host authorizes it."}`
	filePathProp    = `"path":{"type":"string","description":"Absolute path, or a path relative to the context's working directory (at most 4096 bytes)."}`
	fileTimeoutProp = `"timeout_ms":{"type":"integer","minimum":100,"maximum":60000,"description":"Hard deadline in milliseconds (100 to 60000, default 5000)."}`
	fileDigestProp  = `"expected_sha256":{"type":"string","description":"Lowercase SHA-256 of the file's current bytes, from mesh_file_read or an earlier write result."}`
)

func fileSchema(required string, properties ...string) json.RawMessage {
	body := fileHostProp + "," + fileContextProp + "," + filePathProp
	for _, property := range properties {
		body += "," + property
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":[` + required + `],"properties":{` + body + `,` + fileTimeoutProp + `}}`)
}

// fileToolSpecs are the M6 tools: files, copy, fleet and profile.
func fileToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "mesh_file_read",
			Title:       "Read a page of a regular file",
			Operation:   "files.read",
			Group:       groupFiles,
			Description: "Read one page (at most 1 MiB) of a regular file on a host as the daemon account. The page is utf-8 text when exact, otherwise base64, and a text page never splits a character. Offsets are decimal strings. nextCursor continues the read and is bound to the host, account, path and file revision, so a replaced or changed file answers file.changed. sha256 (files up to 4 MiB, or any size with digest) is what mesh_file_write and mesh_file_patch need. Symlinks, devices, sockets and pipes are refused.",
			InputSchema: fileSchema(`"path"`,
				`"offset":{"type":"string","description":"First byte to read, as a nonnegative decimal string (default 0). Not with cursor."}`,
				`"max_bytes":{"type":"integer","minimum":1,"maximum":1048576,"description":"Page size in bytes (1 to 1048576, default 1048576)."}`,
				`"cursor":{"type":"string","description":"Opaque nextCursor from the previous page of this file."}`,
				`"digest":{"type":"boolean","description":"Also hash a file larger than 4 MiB."}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleFileRead,
		},
		{
			Name:        "mesh_file_list",
			Title:       "List a directory",
			Operation:   "files.list",
			Group:       groupFiles,
			Description: "List one directory on a host in name order, without following symlinks. Entries report file, dir, symlink or other. Pages hold up to 1000 entries; nextCursor continues after the last name returned.",
			InputSchema: fileSchema(`"path"`,
				`"limit":{"type":"integer","minimum":1,"maximum":1000,"description":"Entries per page (1 to 1000, default 200)."}`,
				`"cursor":{"type":"string","description":"Opaque nextCursor from the previous page of this directory."}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleFileList,
		},
		{
			Name:        "mesh_file_search",
			Title:       "Search text files with a regular expression",
			Operation:   "files.search",
			Group:       groupFiles,
			Description: "Find lines matching a Go regular expression in the text files under a directory (or in one file) on a host. Never runs a shell or follows symlinks. Bounded: at most 10,000 directory entries visited, 1,000 matches and 1 MiB of encoded matches returned, 256 MiB read, files over 4 MiB and binary files skipped. truncated names the bound that stopped the search.",
			InputSchema: fileSchema(`"path","pattern"`,
				`"pattern":{"type":"string","description":"Go regular expression (RE2 syntax) matched against each line."}`,
				`"glob":{"type":"string","description":"Only search files whose base name matches this glob."}`,
				`"max_matches":{"type":"integer","minimum":1,"maximum":1000,"description":"Stop after this many matches (1 to 1000, default 1000)."}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleFileSearch,
		},
		{
			Name:        "mesh_file_write",
			Title:       "Write or create a file atomically",
			Operation:   "files.write",
			Group:       groupFiles,
			Description: "Replace or create a regular file on a host with at most 1 MiB, through a temporary file and an atomic install in the same directory. Give exactly one of expected_sha256 (replace a file whose digest matches) or create_only (fail if the name exists). A failed precondition answers file.conflict and changes nothing. The result's revision and sha256 describe the version this call installed. Use mesh_copy for larger transfers.",
			InputSchema: fileSchema(`"path"`,
				`"content":{"type":"string","description":"New contents as UTF-8 text. Not with content_base64."}`,
				`"content_base64":{"type":"string","description":"New contents as standard base64 for binary data. Not with content."}`,
				fileDigestProp,
				`"create_only":{"type":"boolean","description":"Create the file and fail with file.conflict if it exists."}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleFileWrite,
		},
		{
			Name:        "mesh_file_patch",
			Title:       "Apply exact text edits to a file",
			Operation:   "files.patch",
			Group:       groupFiles,
			Description: "Apply up to 64 exact old_text to new_text edits, in order, to a file of at most 4 MiB whose SHA-256 matches expected_sha256. Each old_text must occur exactly once in the text as earlier edits left it (overlapping occurrences count as several); otherwise file.patch_mismatch answers and nothing is written.",
			InputSchema: fileSchema(`"path","expected_sha256","edits"`,
				fileDigestProp,
				`"edits":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","additionalProperties":false,"required":["old_text","new_text"],"properties":{"old_text":{"type":"string","minLength":1,"description":"Exact text to replace; it must occur once."},"new_text":{"type":"string","description":"Replacement text, possibly empty."}}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleFilePatch,
		},
		{
			Name:        "mesh_copy",
			Title:       "Copy a file or directory to or from a host",
			Operation:   "files.copy",
			Group:       groupFiles,
			Description: "Copy a file or directory between this machine and one remote Mesh host over an authenticated connection, with no size limit beyond disk space. upload sends local_path to remote_path; download fetches remote_path to local_path. Files are replaced atomically, directories merge, permission bits are kept, and symlinks and special files are refused. Use mesh_file_write for small files.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["host","direction","local_path","remote_path"],"properties":{"host":{"type":"string","description":"Machine name or exact host ID of the remote host."},"direction":{"type":"string","enum":["upload","download"],"description":"upload: local to host. download: host to local."},"local_path":{"type":"string","description":"Absolute path on the machine running mesh mcp."},"remote_path":{"type":"string","description":"Absolute path on the host."},"recursive":{"type":"boolean","description":"Copy a directory and its contents."},"timeout_ms":{"type":"integer","minimum":100,"maximum":600000,"description":"Hard deadline in milliseconds (100 to 600000, default 120000)."}}}`),
			Annotations: destructiveAnnotations(true),
			Handler:     handleCopy,
		},
		{
			Name:        "mesh_fleet_status",
			Title:       "Read signed fleet profile delivery status",
			Operation:   "profile.status",
			Group:       groupFleet,
			Description: "Read this machine's signed fleet profile replica and the delivery state to each saved fleet member, from local files only. Never contacts members.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleFleetStatus,
		},
		{
			Name:        "mesh_profile_get",
			Title:       "Read this machine's host or fleet notes",
			Operation:   "profile.show",
			Group:       groupProfile,
			Description: "Read the owner-written notes of this machine (scope host) or the signed fleet notes replica (scope fleet) without contacting peers. The revision is what mesh_profile_set needs as expected_revision; it is null for an absent profile.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"scope":{"type":"string","enum":["host","fleet"],"description":"host (default) or fleet."}}}`),
			Annotations: readOnlyAnnotations(),
			Handler:     handleProfileGet,
		},
		{
			Name:        "mesh_profile_set",
			Title:       "Replace this machine's host or fleet notes",
			Operation:   "profile.set",
			Group:       groupProfile,
			Description: "Replace the notes of this machine (scope host) or publish the signed fleet notes (scope fleet, from a fleet administrator machine) at an expected revision. expected_revision is required (0 for an absent profile); a stale revision answers profile.conflict and changes nothing. The profile document is replaced whole, not merged. Fleet notes are queued to saved members and then delivered; mesh_fleet_status shows delivery.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["expected_revision","profile"],"properties":{"scope":{"type":"string","enum":["host","fleet"],"description":"host (default) or fleet."},"expected_revision":{"type":"integer","minimum":0,"description":"Revision read from mesh_profile_get; 0 for an absent profile."},"profile":{"type":"object","additionalProperties":false,"required":["schemaVersion"],"properties":{"schemaVersion":{"type":"integer","enum":[1]},"description":{"type":"string","description":"Short description (at most 1024 bytes for a host)."},"labels":{"type":"object","additionalProperties":{"type":"string"},"description":"Up to 32 labels for a host."},"warnings":{"type":"array","items":{"type":"string"},"description":"Up to 16 warnings of at most 512 bytes."},"instructions":{"type":"string","description":"Markdown instructions (at most 16 KiB for a host)."}}}}}`),
			Annotations: destructiveAnnotations(false),
			Handler:     handleProfileSet,
		},
	}
}
