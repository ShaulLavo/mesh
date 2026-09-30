package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/spf13/cobra"
)

type appFlags struct {
	command, setup string
	port           int
	env            []string
}
type appOutput struct {
	json    bool
	sshPort uint
}

func (a *application) appCommand() *cobra.Command {
	flags := &appOutput{sshPort: uint(bootstrap.DefaultSSHPort)}
	cmd := &cobra.Command{Use: "app", Short: "Create and manage disposable websites", Example: "  mesh app create pc ./site\n  mesh app create local ./backend --run 'bun run start' --port 3000\n  mesh app public pc 7k3d\n  mesh app browser approve pc CODE"}
	cmd.PersistentFlags().BoolVar(&flags.json, "json", false, "print machine-readable results")
	cmd.PersistentFlags().UintVar(&flags.sshPort, "ssh-port", uint(bootstrap.DefaultSSHPort), "Mesh SSH port for remote app hosts")
	cmd.AddCommand(a.appCreateCommand(flags, false), a.appCreateCommand(flags, true), a.appListCommand(flags), a.appDownloadCommand(flags))
	for _, operation := range []string{"inspect", "public", "private", "renew", "delete"} {
		cmd.AddCommand(a.appActionCommand(flags, operation))
	}
	browser := &cobra.Command{Use: "browser", Short: "Approve and revoke owner browsers"}
	browser.AddCommand(a.appBrowserCommand(flags, "approve"), a.appBrowserCommand(flags, "list"), a.appBrowserCommand(flags, "revoke"))
	cmd.AddCommand(browser)
	return cmd
}
func (a *application) appCreateCommand(output *appOutput, update bool) *cobra.Command {
	flags := &appFlags{}
	verb, use, n := "create", "create HOST DIR", 2
	if update {
		verb, use, n = "update", "update HOST ID DIR", 3
	}
	cmd := &cobra.Command{Use: use, Short: verb + " a private temporary app from a directory", Args: cobra.ExactArgs(n), RunE: func(cmd *cobra.Command, args []string) error {
		directory := args[len(args)-1]
		request, err := flags.recipe()
		if err != nil {
			return err
		}
		request.Action = verb
		if update {
			request.ID = args[1]
		}
		if update && !appspkg.ValidID(request.ID) {
			return errors.New("app ID must be four lowercase characters")
		}
		transport, err := a.appConnection(cmd.Context(), args[0], output)
		if err != nil {
			return err
		}
		defer transport.close() //nolint:errcheck // the request outcome is authoritative
		result, err := uploadApp(cmd.Context(), transport, directory, request)
		if err != nil {
			return err
		}
		return writeAppResult(cmd.OutOrStdout(), args[0], result, output.json)
	}}
	cmd.Flags().StringVar(&flags.command, "run", "", "explicit HTTP server command; omit for static files")
	cmd.Flags().StringVar(&flags.setup, "setup", "", "explicit setup command in the managed workspace")
	cmd.Flags().IntVar(&flags.port, "port", 0, "HTTP server port; required with --run")
	cmd.Flags().StringArrayVar(&flags.env, "env", nil, "server environment variable NAME=VALUE; repeatable")
	return cmd
}
func (f appFlags) recipe() (appspkg.Request, error) {
	r := appspkg.Request{Kind: "static", Command: f.command, Setup: f.setup, Port: f.port, Env: f.env}
	if f.command != "" {
		r.Kind = "server"
	}
	if f.command != "" && (f.port < 1024 || f.port > 65535) {
		return r, errors.New("--run requires --port from 1024 to 65535")
	}
	if f.command == "" && f.port != 0 {
		return r, errors.New("--port requires an explicit --run command")
	}
	for _, env := range f.env {
		name, _, ok := strings.Cut(env, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \r\n\x00") {
			return r, errors.New("--env requires NAME=VALUE")
		}
	}
	return r, nil
}
func (a *application) appConnection(ctx context.Context, host string, output *appOutput) (appTransport, error) {
	if output.sshPort < 1 || output.sshPort > 65535 {
		return appTransport{}, errors.New("SSH port must be from 1 to 65535")
	}
	return a.openApps(ctx, host, uint16(output.sshPort))
}
func (a *application) appListCommand(output *appOutput) *cobra.Command {
	return &cobra.Command{Use: "list HOST", Aliases: []string{"ls"}, Short: "List apps owned by a host", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.runAppAction(cmd, args[0], appspkg.Request{Action: "list"}, output)
	}}
}
func (a *application) appActionCommand(output *appOutput, operation string) *cobra.Command {
	cmd := &cobra.Command{Use: operation + " HOST ID", Short: operation + " a temporary app", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if !appspkg.ValidID(args[1]) {
			return errors.New("app ID must be four lowercase characters")
		}
		return a.runAppAction(cmd, args[0], appspkg.Request{Action: operation, ID: args[1]}, output)
	}}
	if operation == "inspect" {
		cmd.Aliases = []string{"info"}
	}
	return cmd
}
func (a *application) appBrowserCommand(output *appOutput, operation string) *cobra.Command {
	use, n := operation+" HOST ID", 2
	if operation == "approve" {
		use = operation + " HOST CODE"
	}
	if operation == "list" {
		use, n = operation+" HOST", 1
	}
	return &cobra.Command{Use: use, Short: operation + " owner browser grants", Args: cobra.ExactArgs(n), RunE: func(cmd *cobra.Command, args []string) error {
		r := appspkg.Request{Action: "browser." + operation}
		if operation == "approve" {
			r.Code = args[1]
		}
		if operation == "revoke" {
			r.BrowserID = args[1]
		}
		return a.runAppAction(cmd, args[0], r, output)
	}}
}
func (a *application) runAppAction(cmd *cobra.Command, host string, request appspkg.Request, output *appOutput) error {
	transport, err := a.appConnection(cmd.Context(), host, output)
	if err != nil {
		return err
	}
	defer transport.close() //nolint:errcheck // the request outcome is authoritative
	result, err := transport.request(cmd.Context(), request)
	if err != nil {
		return err
	}
	return writeAppResult(cmd.OutOrStdout(), host, result, output.json)
}
func uploadApp(ctx context.Context, transport appTransport, directory string, request appspkg.Request) (appspkg.Result, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return appspkg.Result{}, err
	}
	archive := &appLimitedBuffer{limit: appspkg.MaxArchive}
	digest, err := appspkg.Pack(ctx, root, archive)
	if err != nil {
		return appspkg.Result{}, err
	}
	begin, err := transport.request(ctx, appspkg.Request{Action: "upload.begin"})
	if err != nil {
		return appspkg.Result{}, err
	}
	if len(begin.UploadID) != 43 {
		return appspkg.Result{}, errors.New("host returned an invalid upload identifier")
	}
	reader := bytes.NewReader(archive.Bytes())
	var offset int64
	for reader.Len() > 0 {
		chunk := make([]byte, min(appspkg.ChunkSize, reader.Len()))
		if _, err = io.ReadFull(reader, chunk); err != nil {
			return appspkg.Result{}, err
		}
		_, err = transport.request(ctx, appspkg.Request{Action: "upload.chunk", UploadID: begin.UploadID, Offset: offset, Data: chunk})
		if err != nil {
			return appspkg.Result{}, err
		}
		offset += int64(len(chunk))
	}
	request.UploadID = begin.UploadID
	request.Digest = digest
	return transport.request(ctx, request)
}
func (a *application) appDownloadCommand(output *appOutput) *cobra.Command {
	return &cobra.Command{Use: "download HOST ID DEST", Short: "Save an owner's app source archive", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		if !appspkg.ValidID(args[1]) {
			return errors.New("app ID must be four lowercase characters")
		}
		transport, err := a.appConnection(cmd.Context(), args[0], output)
		if err != nil {
			return err
		}
		defer transport.close() //nolint:errcheck // the request outcome is authoritative
		if err = downloadApp(cmd.Context(), transport, args[1], args[2]); err != nil {
			return err
		}
		if output.json {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"output": args[2]})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "saved %s\n", args[2])
		return err
	}}
}
func downloadApp(ctx context.Context, transport appTransport, id, dest string) error {
	absolute, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(absolute), ".mesh-app-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // remove the unlinked transfer file
	defer tmp.Close()           //nolint:errcheck // checked before publishing the file
	var offset int64
	for {
		result, err := transport.request(ctx, appspkg.Request{Action: "download", ID: id, Offset: offset})
		if err != nil {
			return err
		}
		if len(result.Data) > appspkg.ChunkSize || offset+int64(len(result.Data)) > appspkg.MaxArchive {
			return errors.New("download exceeds app archive limit")
		}
		if len(result.Data) == 0 && !result.Done {
			return errors.New("host returned a stalled app download")
		}
		if _, err = tmp.Write(result.Data); err != nil {
			return err
		}
		offset += int64(len(result.Data))
		if result.Done {
			break
		}
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Link publishes atomically and refuses to replace an existing destination.
	return os.Link(tmp.Name(), absolute)
}
func writeAppResult(w io.Writer, host string, result appspkg.Result, asJSON bool) error {
	if asJSON {
		value := struct {
			appspkg.Result
			Host string `json:"host"`
			URL  string `json:"url,omitempty"`
		}{Result: result, Host: host}
		if result.App != nil {
			value.URL = appspkg.URL(result.App.ID)
		}
		return json.NewEncoder(w).Encode(value)
	}
	if len(result.Browsers) > 0 {
		_, err := fmt.Fprintln(w, string(result.Browsers))
		return err
	}
	if result.App != nil {
		return writeOneApp(w, host, *result.App)
	}
	if result.Apps != nil {
		table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(table, "ID\tVISIBILITY\tSTATE\tEXPIRES\tURL")
		for _, app := range result.Apps {
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", app.ID, app.Visibility, app.Status, app.ExpiresAt.Format("2006-01-02 15:04 MST"), appspkg.URL(app.ID))
		}
		return table.Flush()
	}
	_, err := fmt.Fprintln(w, "done")
	return err
}
func writeOneApp(w io.Writer, host string, app appspkg.Record) error {
	_, err := fmt.Fprintf(w, "%s\nhost: %s\nowner: %s\nvisibility: %s\nstate: %s\nexpires: %s\n", appspkg.URL(app.ID), SafeTerminalText(host), SafeTerminalText(app.Owner), SafeTerminalText(app.Visibility), SafeTerminalText(app.Status), app.ExpiresAt.Format("2006-01-02 15:04:05 MST"))
	return err
}
