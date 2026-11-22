package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/protocol"
)

// fileControlCommand is a hidden diagnostic surface over HostFiles. The native
// MCP file tools call the same helper, so this is the way to try a daemon's
// file controls by hand and to test them end to end.
func (a *application) fileControlCommand() *cobra.Command {
	var host string
	group := &cobra.Command{Use: "file-control", Hidden: true, Short: "Run a daemon file control on one host"}
	group.PersistentFlags().StringVar(&host, "host", "", "host name or exact ID")
	group.AddCommand(a.fileReadCommand(&host), a.fileListCommand(&host), a.fileSearchCommand(&host), a.fileWriteCommand(&host), a.filePatchCommand(&host))
	return group
}

func (a *application) fileReadCommand(host *string) *cobra.Command {
	var request protocol.FileReadRequest
	command := &cobra.Command{Use: "read PATH", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		request.Path = args[0]
		return a.runFileControl(cmd, *host, func(ctx context.Context, files HostFiles) (any, error) { return files.Read(ctx, request) })
	}}
	command.Flags().Uint64Var(&request.Offset, "offset", 0, "first byte to read")
	command.Flags().IntVar(&request.MaxBytes, "max-bytes", 0, "page size, at most 1 MiB")
	command.Flags().StringVar(&request.Cursor, "cursor", "", "continue from a previous page")
	command.Flags().BoolVar(&request.Digest, "digest", false, "hash a file larger than 4 MiB")
	return command
}

func (a *application) fileListCommand(host *string) *cobra.Command {
	var request protocol.FileListRequest
	command := &cobra.Command{Use: "list PATH", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		request.Path = args[0]
		return a.runFileControl(cmd, *host, func(ctx context.Context, files HostFiles) (any, error) { return files.List(ctx, request) })
	}}
	command.Flags().IntVar(&request.Limit, "limit", 0, "entries per page")
	command.Flags().StringVar(&request.Cursor, "cursor", "", "continue from a previous page")
	return command
}

func (a *application) fileSearchCommand(host *string) *cobra.Command {
	var request protocol.FileSearchRequest
	command := &cobra.Command{Use: "search PATH PATTERN", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		request.Path, request.Pattern = args[0], args[1]
		return a.runFileControl(cmd, *host, func(ctx context.Context, files HostFiles) (any, error) { return files.Search(ctx, request) })
	}}
	command.Flags().StringVar(&request.Glob, "glob", "", "match file base names")
	command.Flags().IntVar(&request.MaxMatches, "max-matches", 0, "stop after this many matches")
	return command
}

func (a *application) fileWriteCommand(host *string) *cobra.Command {
	var request protocol.FileWriteRequest
	var source string
	command := &cobra.Command{Use: "write PATH --file SOURCE", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		request.Path = args[0]
		if err := loadWriteData(cmd.InOrStdin(), source, &request); err != nil {
			return err
		}
		return a.runFileControl(cmd, *host, func(ctx context.Context, files HostFiles) (any, error) { return files.Write(ctx, request) })
	}}
	command.Flags().StringVar(&source, "file", "", "local file with the new contents, or - for standard input")
	command.Flags().StringVar(&request.ExpectedSHA256, "expected-sha256", "", "digest the existing file must have")
	command.Flags().BoolVar(&request.CreateOnly, "create-only", false, "create the file and fail when it exists")
	_ = command.MarkFlagRequired("file")
	return command
}

func (a *application) filePatchCommand(host *string) *cobra.Command {
	var request protocol.FilePatchRequest
	var oldTexts, newTexts []string
	command := &cobra.Command{Use: "patch PATH", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(oldTexts) == 0 || len(oldTexts) != len(newTexts) {
			return errors.New("give each --old with a matching --new")
		}
		request.Path = args[0]
		for i := range oldTexts {
			request.Edits = append(request.Edits, protocol.FileEdit{OldText: oldTexts[i], NewText: newTexts[i]})
		}
		return a.runFileControl(cmd, *host, func(ctx context.Context, files HostFiles) (any, error) { return files.Patch(ctx, request) })
	}}
	command.Flags().StringVar(&request.ExpectedSHA256, "expected-sha256", "", "digest the file must have")
	command.Flags().StringArrayVar(&oldTexts, "old", nil, "exact text to replace; repeat for more edits")
	command.Flags().StringArrayVar(&newTexts, "new", nil, "replacement text")
	return command
}

// loadWriteData reads at most the write limit and picks the encoding the
// daemon will decode exactly.
func loadWriteData(stdin io.Reader, source string, request *protocol.FileWriteRequest) error {
	reader := stdin
	if source != "-" {
		file, err := os.Open(source) //nolint:gosec // the caller names the local source file
		if err != nil {
			return fmt.Errorf("open write source: %w", err)
		}
		defer file.Close() //nolint:errcheck // The source is read-only.
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, protocol.MaxFileWrite+1))
	if err != nil {
		return fmt.Errorf("read write source: %w", err)
	}
	if len(data) > protocol.MaxFileWrite {
		return fmt.Errorf("write source is larger than %d bytes; use mesh cp", protocol.MaxFileWrite)
	}
	request.Encoding, request.Data = protocol.FileEncodingUTF8, string(data)
	// Text whose JSON escaping would outgrow the frame travels as Base64.
	if escaped, err := json.Marshal(request.Data); !utf8.Valid(data) || err != nil || len(escaped) > protocol.MaxFileTextEncoded {
		request.Encoding, request.Data = protocol.FileEncodingBase64, base64.StdEncoding.EncodeToString(data)
	}
	return nil
}

// runFileControl opens one verified connection, checks that the host serves
// file controls, runs the operation and prints its result as JSON.
func (a *application) runFileControl(cmd *cobra.Command, hostName string, run func(context.Context, HostFiles) (any, error)) error {
	if hostName == "" {
		return errors.New("name the host with --host")
	}
	ctx := cmd.Context()
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	host, err := resolveHostTarget(hosts, hostName)
	if err != nil {
		return err
	}
	conn, info, err := openVerifiedHostInfo(ctx, host, a.dependencies.DialControl)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // The operation result decides success.
	if err := FileControlsSupported(info); err != nil {
		return err
	}
	result, err := run(ctx, HostFiles{Conn: conn, Target: protocol.FileTarget{HostID: info.ID}})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("write file control result: %w", err)
	}
	return nil
}
