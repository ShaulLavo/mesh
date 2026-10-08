package cli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/privacy"
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
	cmd.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if root := command.Root(); root.PersistentPreRunE != nil {
			if err := root.PersistentPreRunE(command, args); err != nil {
				return fmt.Errorf("app: configure client: %w", err)
			}
		}
		if domainpolicy.Primary() == "" {
			return errors.New("app: configure domains.json before managing websites")
		}
		return nil
	}
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
	verb, use, n := "create", "create HOST SOURCE...", 2
	if update {
		verb, use, n = "update", "update HOST ID SOURCE...", 3
	}
	cmd := &cobra.Command{Use: use, Short: verb + " a temporary app from files or a directory", Args: cobra.MinimumNArgs(n), RunE: func(cmd *cobra.Command, args []string) error {
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
		return a.publishAppSources(cmd, args[0], args[n-1:], request, output)
	}}
	cmd.Flags().StringVar(&flags.command, "run", "", "explicit HTTP server command; omit for static files")
	cmd.Flags().StringVar(&flags.setup, "setup", "", "explicit setup command in the managed workspace")
	cmd.Flags().IntVar(&flags.port, "port", 0, "HTTP server port; required with --run")
	cmd.Flags().StringArrayVar(&flags.env, "env", nil, "server environment variable NAME=VALUE; repeatable")
	return cmd
}
func (a *application) publishAppSources(cmd *cobra.Command, host string, sources []string, request appspkg.Request, output *appOutput) error {
	if len(sources) == 1 {
		info, err := os.Stat(sources[0])
		if err != nil {
			return fmt.Errorf("app: inspect source: %w", err)
		}
		if info.IsDir() {
			return a.publishApp(cmd, host, sources[0], request, output)
		}
	}
	if request.Kind == "server" || request.Setup != "" {
		return errors.New("--run and --setup require a single source directory")
	}
	directory, err := appspkg.PreparePage(cmd.Context(), sources)
	if err != nil {
		return fmt.Errorf("app: prepare page: %w", err)
	}
	defer os.RemoveAll(directory) //nolint:errcheck // Remove the disposable staging copy after upload or failure.
	return a.publishApp(cmd, host, directory, request, output)
}
func (a *application) publishApp(cmd *cobra.Command, host, directory string, request appspkg.Request, output *appOutput) error {
	transport, err := a.appConnection(cmd.Context(), host, output)
	if err != nil {
		return err
	}
	defer transport.close() //nolint:errcheck // the request outcome is authoritative
	result, err := uploadApp(cmd.Context(), transport, directory, request)
	if err != nil {
		return err
	}
	return writeAppResult(cmd.OutOrStdout(), host, result, output.json, a.privacy)
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

const browserApproveOperation = "approve"
const browserYesFlag = "yes"

func (a *application) appBrowserCommand(output *appOutput, operation string) *cobra.Command {
	use, n := operation+" HOST ID", 2
	if operation == browserApproveOperation {
		use = operation + " HOST CODE"
	}
	if operation == "list" {
		use, n = operation+" HOST", 1
	}
	var yes bool
	command := &cobra.Command{Use: use, Short: operation + " owner browser grants", Args: cobra.ExactArgs(n), RunE: func(cmd *cobra.Command, args []string) error {
		if operation == browserApproveOperation {
			return a.runBrowserApproval(cmd, args[0], args[1], output, yes)
		}
		r := appspkg.Request{Action: "browser." + operation}
		if operation == "revoke" {
			r.BrowserID = args[1]
		}
		return a.runAppAction(cmd, args[0], r, output)
	}}
	if operation == browserApproveOperation {
		command.Flags().BoolVar(&yes, browserYesFlag, false, "skip the human browser confirmation for scripts")
	}
	return command
}

func (a *application) runBrowserApproval(cmd *cobra.Command, host, code string, output *appOutput, yes bool) error {
	transport, err := a.appConnection(cmd.Context(), host, output)
	if err != nil {
		return err
	}
	defer transport.close() //nolint:errcheck // the request outcome is authoritative
	inspection, err := transport.request(cmd.Context(), appspkg.Request{Action: "browser.inspect", Code: code})
	if err != nil {
		return fmt.Errorf("inspect pending browser on %s: %w", host, err)
	}
	if inspection.Pairing == nil {
		return errors.New("host did not return pending browser details; update Mesh before approving")
	}
	info := inspection.Pairing
	if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "User-Agent: %s\nSource IP: %s\nPending age: %s\nOnly approve a code you requested in your own browser. Browser details are not proof of identity.\n", browserPresentation(a.privacy, info.UserAgent), a.privacy.Value("ip", info.SourceIP), (time.Duration(info.AgeSeconds) * time.Second).String()); err != nil {
		return fmt.Errorf("show pending browser details: %w", err)
	}
	confirmed, err := confirmBrowserApproval(cmd, yes)
	if err != nil || !confirmed {
		return err
	}
	result, err := transport.request(cmd.Context(), appspkg.Request{Action: "browser.approve", Code: code})
	if err != nil {
		return err
	}
	return writeAppResult(cmd.OutOrStdout(), host, result, output.json, a.privacy)
}
func confirmBrowserApproval(cmd *cobra.Command, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return false, errors.New("browser approval requires interactive confirmation; --yes skips the human check for scripts")
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), "Approve this browser? [y/N] "); err != nil {
		return false, fmt.Errorf("show browser approval prompt: %w", err)
	}
	answer, err := bufio.NewReader(io.LimitReader(input, 1024)).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read browser approval confirmation: %w", err)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "y" || answer == browserYesFlag {
		return true, nil
	}
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "browser approval cancelled"); err != nil {
		return false, fmt.Errorf("show browser approval cancellation: %w", err)
	}
	return false, nil
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
	return writeAppResult(cmd.OutOrStdout(), host, result, output.json, a.privacy)
}
func uploadApp(ctx context.Context, transport appTransport, directory string, request appspkg.Request) (appspkg.Result, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return appspkg.Result{}, err
	}
	// Pack refuses an archive over the limit the host enforces.
	archive := &bytes.Buffer{}
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
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"output": a.privacy.Value("path", args[2])})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "saved %s\n", a.privacy.Value("path", args[2]))
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
	if err = checkDownloadedArchive(tmp); err != nil {
		return fmt.Errorf("app %s download is not one intact archive, perhaps because its source changed during the transfer; run it again: %w", id, err)
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Link publishes atomically and refuses to replace an existing destination.
	return os.Link(tmp.Name(), absolute)
}

// checkDownloadedArchive reads the archive through gzip, whose checksum fails
// when the host served chunks from two different snapshots. Expansion is
// bounded so a host cannot make the check itself unbounded work.
func checkDownloadedArchive(f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind download: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read download: %w", err)
	}
	limit := int64(2 * appspkg.MaxArchive)
	n, err := io.Copy(io.Discard, io.LimitReader(gz, limit+1))
	if err != nil {
		return fmt.Errorf("read download: %w", err)
	}
	if n > limit {
		return errors.New("download expands past the source limit")
	}
	return nil
}
func writeAppResult(w io.Writer, host string, result appspkg.Result, asJSON bool, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	if asJSON {
		value := struct {
			appspkg.Result
			Host string `json:"host"`
			URL  string `json:"url,omitempty"`
		}{Result: privateAppResult(mask, result), Host: mask.Value("host", host)}
		if result.App != nil {
			value.URL = mask.Value("url", appspkg.URL(result.App.ID))
		}
		return json.NewEncoder(w).Encode(value)
	}
	if len(result.Browsers) > 0 {
		_, err := fmt.Fprintln(w, browserPresentation(mask, string(result.Browsers)))
		return err
	}
	if result.App != nil {
		return writeAppDetails(w, host, result, mask)
	}

	if result.Apps != nil {
		table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(table, "ID\tVISIBILITY\tSTATE\tEXPIRES\tURL")
		for _, app := range result.Apps {
			_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", mask.Value("app", app.ID), app.Visibility, app.Status, app.ExpiresAt.Format("2006-01-02 15:04 MST"), mask.Value("url", appspkg.URL(app.ID)))
		}
		return table.Flush()
	}
	_, err := fmt.Fprintln(w, "done")
	return err
}

func writeAppDetails(w io.Writer, host string, result appspkg.Result, mask *privacy.Mask) error {
	if err := writeOneApp(w, host, *result.App, mask); err != nil {
		return err
	}
	if mask != nil {
		return writePrivateAppRuntime(w, result.Runtime)
	}
	if err := writeSetupFailure(w, result.Runtime); err != nil {
		return err
	}
	return writeServingProblem(w, result.Runtime)
}

func writePrivateAppRuntime(w io.Writer, runtime *appspkg.RuntimeInfo) error {
	if runtime == nil {
		return nil
	}
	if runtime.Failure != nil {
		if _, err := fmt.Fprintln(w, "setup failed: [build details withheld]"); err != nil {
			return fmt.Errorf("show private setup failure: %w", err)
		}
	}
	if runtime.Problem != "" {
		if _, err := fmt.Fprintln(w, "not serving: [details withheld]"); err != nil {
			return fmt.Errorf("show private serving problem: %w", err)
		}
	}
	return nil
}

// writeServingProblem says why the host stopped serving the app, when it did.
func writeServingProblem(w io.Writer, runtime *appspkg.RuntimeInfo) error {
	if runtime == nil || runtime.Problem == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "not serving: %s\n", SafeTerminalText(runtime.Problem)); err != nil {
		return fmt.Errorf("show serving problem: %w", err)
	}
	return nil
}
func writeSetupFailure(w io.Writer, runtime *appspkg.RuntimeInfo) error {
	if runtime == nil || runtime.Failure == nil {
		return nil
	}
	failure := runtime.Failure
	if _, err := fmt.Fprintf(w, "setup failed: %s\n", SafeTerminalText(failure.Error)); err != nil {
		return fmt.Errorf("show setup failure: %w", err)
	}
	for _, line := range strings.Split(strings.TrimRight(failure.Output, "\n"), "\n") {
		if line == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %s\n", SafeTerminalText(line)); err != nil {
			return fmt.Errorf("show setup output: %w", err)
		}
	}
	return nil
}
func writeOneApp(w io.Writer, host string, app appspkg.Record, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	// The sanitized URL authority no longer carries the actionable app ID.
	if mask != nil {
		if _, err := fmt.Fprintf(w, "id: %s\n", mask.Value("app", app.ID)); err != nil {
			return fmt.Errorf("show private app ID: %w", err)
		}
	}
	_, err := fmt.Fprintf(w, "%s\nhost: %s\nowner: %s\nvisibility: %s\nstate: %s\nexpires: %s\n", mask.Value("url", appspkg.URL(app.ID)), SafeTerminalText(mask.Value("host", host)), SafeTerminalText(mask.Value("owner", app.Owner)), SafeTerminalText(app.Visibility), SafeTerminalText(app.Status), app.ExpiresAt.Format("2006-01-02 15:04:05 MST"))
	return err
}

// Arbitrary payloads can contain secrets that metadata recognition cannot find.
func browserPresentation(mask *privacy.Mask, payload string) string {
	if mask != nil {
		return "[browser details withheld]"
	}
	return payload
}

func presentationMask(masks []*privacy.Mask) *privacy.Mask {
	if len(masks) == 0 {
		return nil
	}
	return masks[0]
}

// Clone nested records before masking: the result also carries authoritative
// identifiers and payloads used by control operations.
func privateAppResult(mask *privacy.Mask, result appspkg.Result) appspkg.Result {
	if mask == nil {
		return result
	}
	if result.App != nil {
		app := privateAppRecord(mask, *result.App)
		result.App = &app
	}
	if result.Apps != nil {
		result.Apps = append([]appspkg.Record{}, result.Apps...)
		for i := range result.Apps {
			result.Apps[i] = privateAppRecord(mask, result.Apps[i])
		}
	}
	if result.Pairing != nil {
		pairing := *result.Pairing
		pairing.UserAgent = browserPresentation(mask, pairing.UserAgent)
		pairing.SourceIP = mask.Value("ip", pairing.SourceIP)
		result.Pairing = &pairing
	}
	result.Runtime = privateAppRuntime(mask, result.Runtime)
	result.UploadID = mask.Value("upload", result.UploadID)
	if len(result.Data) > 0 {
		result.Data = []byte("[data withheld]")
	}
	// Browser responses are intentionally unstructured. Keep the result field
	// present as JSON null rather than guessing which nested properties are safe.
	if len(result.Browsers) > 0 {
		result.Browsers = json.RawMessage("null")
	}
	return result
}

func privateAppRecord(mask *privacy.Mask, app appspkg.Record) appspkg.Record {
	app.ID = mask.Value("app", app.ID)
	app.Owner = mask.Value("owner", app.Owner)
	app.Revision = mask.Value("revision", app.Revision)
	return app
}

func privateAppRuntime(mask *privacy.Mask, original *appspkg.RuntimeInfo) *appspkg.RuntimeInfo {
	if original == nil {
		return nil
	}
	runtime := *original
	runtime.SessionID = mask.Value("session", runtime.SessionID)
	runtime.Root = mask.Value("path", runtime.Root)
	if runtime.Command != "" {
		runtime.Command = "[command withheld]"
	}
	if runtime.Problem != "" {
		runtime.Problem = "[details withheld]"
	}
	runtime.Failure = privateAppFailure(mask, runtime.Failure)
	return &runtime
}

func privateAppFailure(mask *privacy.Mask, original *appspkg.SetupFailure) *appspkg.SetupFailure {
	if original == nil {
		return nil
	}
	failure := *original
	failure.UploadID = mask.Value("upload", failure.UploadID)
	if failure.Error != "" {
		failure.Error = "[build details withheld]"
	}
	if failure.Output != "" {
		failure.Output = "[build output withheld]"
	}
	return &failure
}
