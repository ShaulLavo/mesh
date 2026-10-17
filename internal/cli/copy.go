package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transfer"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

type copyEndpoint struct {
	host string
	path string
}

func parseCopyEndpoint(value string) (copyEndpoint, error) {
	host, path, remote := strings.Cut(value, ":")
	if !remote || strings.Contains(host, "/") {
		return copyEndpoint{path: value}, nil
	}
	if host == "" || !filepath.IsAbs(path) {
		return copyEndpoint{}, errors.New("remote paths must use HOST:/absolute/path; prefix local names containing ':' with ./")
	}
	return copyEndpoint{host: host, path: path}, nil
}

func (a *application) copyCommand() *cobra.Command {
	var recursive bool
	command := &cobra.Command{
		Use:   "cp SOURCE DESTINATION",
		Short: "Copy files or directories through an authenticated Mesh connection",
		Long: "Copy between this machine and one enrolled Mesh host.\n" +
			"Use HOST:/absolute/path for the remote path and -r for directories.\n" +
			"An existing destination directory receives the source basename. Files are\n" +
			"replaced atomically, directories merge, and destination-only files stay.\n" +
			"Dotfiles, empty directories, and permission bits are preserved. Symbolic\n" +
			"links and special files are refused. No saved terminal session is created.",
		Example: "  mesh cp ./artifact.wasm upcloud-4:/srv/agent-work/artifact.wasm\n" +
			"  mesh cp -r ./source upcloud-2:/srv/agent-work/checkouts/\n" +
			"  mesh cp -r upcloud-4:/srv/agent-work/results ./results",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.copyFiles(cmd.Context(), args[0], args[1], recursive)
		},
	}
	command.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy a directory and its contents")
	return command
}

func (a *application) copyFiles(ctx context.Context, source, destination string, recursive bool) error {
	src, err := parseCopyEndpoint(source)
	if err != nil {
		return err
	}
	dst, err := parseCopyEndpoint(destination)
	if err != nil {
		return err
	}
	if (src.host == "") == (dst.host == "") {
		return errors.New("mesh cp requires one local path and one HOST:/absolute/path")
	}
	remote := dst
	if src.host != "" {
		remote = src
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	host, err := resolveHostTarget(hosts, remote.host)
	if err != nil {
		return err
	}
	conn, err := openVerifiedHost(ctx, host, a.dependencies.DialControl)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // The copy result determines success.
	if src.host != "" {
		return downloadCopy(ctx, conn, src.path, dst.path, recursive)
	}
	return uploadCopy(ctx, conn, src.path, dst.path, recursive)
}

func copyRequest(ctx context.Context, conn transport.Conn, request transfer.Request) (transfer.Result, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return transfer.Result{}, fmt.Errorf("encode copy request: %w", err)
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return transfer.Result{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeCopyRequest, RequestID: id, Copy: data})
	if err != nil {
		return transfer.Result{}, err
	}
	if response.Type == protocol.TypeError {
		return transfer.Result{}, daemonResponseError("copy", response.Message)
	}
	if response.Type != protocol.TypeCopyResult {
		return transfer.Result{}, errors.New("host returned an unexpected copy response")
	}
	var result transfer.Result
	if err := json.Unmarshal(response.Copy, &result); err != nil {
		return transfer.Result{}, fmt.Errorf("decode copy response: %w", err)
	}
	return result, nil
}

func uploadCopy(ctx context.Context, conn transport.Conn, source, destination string, recursive bool) error {
	stream, err := transfer.OpenSource(ctx, source, recursive)
	if err != nil {
		return fmt.Errorf("open copy source: %w", err)
	}
	defer stream.Close() //nolint:errcheck // The transfer outcome is authoritative.
	_, err = copyRequest(ctx, conn, transfer.Request{Action: "upload.begin", Path: destination, Info: stream.Info})
	if err != nil {
		return err
	}
	var size int64
	err = transfer.Upload(ctx, stream, func(ctx context.Context, offset int64, data []byte) error {
		_, err := copyRequest(ctx, conn, transfer.Request{Action: "upload.chunk", Offset: offset, Data: data})
		size = offset + int64(len(data))
		return err
	})
	if err != nil {
		return err
	}
	info, err := stream.Finish()
	if err != nil {
		return fmt.Errorf("pack copy source: %w", err)
	}
	_, err = copyRequest(ctx, conn, transfer.Request{Action: "upload.commit", Info: info, Size: size})
	return err
}

func downloadCopy(ctx context.Context, conn transport.Conn, source, destination string, recursive bool) error {
	result, err := copyRequest(ctx, conn, transfer.Request{Action: "download.begin", Path: source, Recursive: recursive})
	if err != nil {
		return err
	}
	if result.Info.Directory && !recursive {
		return errors.New("host returned a directory without --recursive")
	}
	archive, err := transfer.Temporary(destination)
	if err != nil {
		return fmt.Errorf("create copy download: %w", err)
	}
	defer archive.Close() //nolint:errcheck // Receive checks its IO.
	var final transfer.Result
	received, err := transfer.Download(ctx, archive, func(ctx context.Context, offset int64) (transfer.Chunk, error) {
		chunk, err := copyRequest(ctx, conn, transfer.Request{Action: "download.chunk", Offset: offset})
		if err != nil {
			return transfer.Chunk{}, err
		}
		if chunk.Done {
			final = chunk
		}
		return transfer.Chunk{Data: chunk.Data, Done: chunk.Done}, nil
	})
	if err != nil {
		return err
	}
	if received != final.Size || result.Info.Name != final.Info.Name || result.Info.Directory != final.Info.Directory {
		return errors.New("copy download returned inconsistent source metadata")
	}
	return transfer.Receive(ctx, archive, final.Info, destination)
}
