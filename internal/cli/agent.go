package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/worker"
)

const agentRequestTimeout = 2 * time.Second

type agentLease struct {
	connection net.Conn
	location   worker.SessionWorkerLocation
	token      string
	hostID     string
}

func (a *application) agentCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "agent PROVIDER -- ARG...", Short: "Run a native agent with conversation recovery", DisableFlagParsing: true,
		Args: cobra.ArbitraryArgs, RunE: a.runAgentCommand,
	}
	command.AddCommand(a.agentBindCommand(), a.agentSetupCommand(), a.agentDoctorCommand())
	return command
}

func (a *application) runAgentCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return cmd.Help()
	}
	if len(args) == 0 || len(args) > 1 && args[1] != "--" {
		return errors.New("use mesh agent PROVIDER -- ARG...")
	}
	provider, err := parseAgentProvider(args[0])
	if err != nil {
		return err
	}
	arguments := args[min(2, len(args)):]
	executable, err := resolveAgentExecutable(provider)
	if err != nil {
		return err
	}
	location, contained := worker.ContainingSessionWorker()
	if !contained {
		return runNativeAgent(cmd, executable, arguments, "", clearAgentInvocationEnv(os.Environ()))
	}
	launch, err := inspectAgentLaunch(cmd.Context(), provider, executable, arguments)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Mesh conversation recovery unavailable: %v\n", err)
		return runNativeAgent(cmd, executable, arguments, "", clearAgentInvocationEnv(os.Environ()))
	}
	if reason := agentresume.Compatibility(launch); reason != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Mesh conversation recovery unavailable: %s\n", reason)
		return runNativeAgent(cmd, executable, arguments, "", clearAgentInvocationEnv(os.Environ()))
	}
	return runRegisteredAgent(cmd, location, launch, arguments, os.Environ(), "", "", false)
}

func parseAgentProvider(name string) (agentresume.Provider, error) {
	provider := agentresume.Provider(name)
	if provider != agentresume.Codex && provider != agentresume.Claude {
		return "", fmt.Errorf("agent provider must be codex or claude, got %q", name)
	}
	return provider, nil
}

func resolveAgentExecutable(provider agentresume.Provider) (string, error) {
	path, err := exec.LookPath(string(provider))
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", provider, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable: %w", provider, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s installation: %w", provider, err)
	}
	return resolved, nil
}

func inspectAgentLaunch(ctx context.Context, provider agentresume.Provider, executable string, arguments []string) (agentresume.Launch, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return agentresume.Launch{}, fmt.Errorf("read agent directory: %w", err)
	}
	version, err := installedAgentVersion(ctx, executable)
	if err != nil {
		return agentresume.Launch{}, err
	}
	return agentresume.ParseLaunch(provider, executable, version, cwd, os.Environ(), arguments)
}

type agentVersionOutput struct{ data []byte }

func (output *agentVersionOutput) Write(data []byte) (int, error) {
	if len(output.data)+len(data) > 4096 {
		return 0, errors.New("provider version output exceeds 4096 bytes")
	}
	output.data = append(output.data, data...)
	return len(data), nil
}

func installedAgentVersion(parent context.Context, executable string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, agentRequestTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--version") //nolint:gosec // resolved native provider executable, fixed read-only argument
	output := &agentVersionOutput{}
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("read provider version: %w", err)
	}
	return strings.TrimSpace(string(output.data)), nil
}

func runRegisteredAgent(cmd *cobra.Command, location worker.SessionWorkerLocation, launch agentresume.Launch, arguments, env []string, directory, expected string, explicit bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentRequestTimeout)
	lease, err := beginAgentInvocation(ctx, location, launch, expected, explicit, false)
	cancel()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Mesh conversation recovery unverified: %v\n", err)
		return runNativeAgent(cmd, launch.Executable, arguments, directory, clearAgentInvocationEnv(env))
	}
	defer lease.connection.Close() //nolint:errcheck // abrupt close retains the durable binding
	result := runNativeAgent(cmd, launch.Executable, arguments, directory, agentInvocationEnv(env, lease))
	verified, finishErr := lease.finish()
	if finishErr != nil || expected != "" && !verified {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Mesh conversation recovery is unverified. The provider did not acknowledge the saved conversation; check mesh agent doctor.")
	}
	return result
}

func beginAgentInvocation(ctx context.Context, location worker.SessionWorkerLocation, launch agentresume.Launch, expected string, explicit, lookupOnly bool) (*agentLease, error) {
	containing, err := awaitAgentWorker(ctx, location)
	if err != nil {
		return nil, fmt.Errorf("identify agent recovery host: %w", err)
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket(location.Dir))
	if err != nil {
		return nil, fmt.Errorf("connect agent recovery worker: %w", err)
	}
	request := protocol.Control{Type: protocol.TypeAgentBegin, SessionID: location.SessionID,
		AgentLaunch: &launch, AgentPID: os.Getpid(), AgentHostID: containing[0].HostID, AgentExpectedID: expected, AgentExplicit: explicit, AgentLookupOnly: lookupOnly}
	response, err := agentLeaseRequest(connection, request)
	if err != nil || response.Type != protocol.TypeAgentBegun || response.SessionID != location.SessionID || response.AgentToken == "" || response.AgentHostID == "" {
		_ = connection.Close()
		return nil, fmt.Errorf("begin agent recovery: %w", agentResponseError(response, err))
	}
	return &agentLease{connection: connection, location: location, token: response.AgentToken, hostID: response.AgentHostID}, nil
}

func awaitAgentWorker(ctx context.Context, location worker.SessionWorkerLocation) ([]protocol.SessionIdentity, error) {
	for {
		containing, err := queryContainingSessionWorker(ctx, location)
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return containing, err
		}
		if err := waitAgentStartup(ctx); err != nil {
			return nil, err
		}
	}
}

func waitAgentStartup(ctx context.Context) error {
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for agent recovery worker: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func agentLeaseRequest(connection net.Conn, request protocol.Control) (protocol.Control, error) {
	if err := connection.SetDeadline(time.Now().Add(agentRequestTimeout)); err != nil {
		return protocol.Control{}, fmt.Errorf("set agent recovery deadline: %w", err)
	}
	defer connection.SetDeadline(time.Time{}) //nolint:errcheck // a lease remains open while the native provider runs
	if err := protocol.NewWriter(connection).WriteControlMsg(request); err != nil {
		return protocol.Control{}, fmt.Errorf("send agent recovery request: %w", err)
	}
	frame, err := protocol.NewReader(connection).ReadFrame()
	if err != nil {
		return protocol.Control{}, fmt.Errorf("read agent recovery acknowledgement: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errors.New("agent recovery acknowledgement is not a control frame")
	}
	return protocol.DecodeControl(frame.Payload)
}

func agentResponseError(response protocol.Control, err error) error {
	if err != nil {
		return err
	}
	if response.Message != "" {
		return errors.New(response.Message)
	}
	return errors.New("worker returned an invalid acknowledgement")
}

func (lease *agentLease) finish() (bool, error) {
	response, err := agentLeaseRequest(lease.connection, protocol.Control{Type: protocol.TypeAgentFinish,
		SessionID: lease.location.SessionID, AgentToken: lease.token})
	if err != nil || response.Type != protocol.TypeOK {
		return false, agentResponseError(response, err)
	}
	return response.AgentVerified, nil
}

func agentInvocationEnv(source []string, lease *agentLease) []string {
	env := clearAgentInvocationEnv(source)
	return append(env, "MESH_AGENT_TOKEN="+lease.token, "MESH_AGENT_SOCKET="+paths.Socket(lease.location.Dir),
		"MESH_AGENT_SESSION_ID="+lease.location.SessionID, "MESH_AGENT_HOST_ID="+lease.hostID)
}

func clearAgentInvocationEnv(source []string) []string {
	env := make([]string, 0, len(source)+4)
	for _, entry := range source {
		if !strings.HasPrefix(entry, "MESH_AGENT_") {
			env = append(env, entry)
		}
	}
	return env
}

func runNativeAgent(cmd *cobra.Command, executable string, arguments []string, directory string, env []string) error {
	restore := preserveAgentTerminal(cmd.InOrStdin())
	defer restore()
	child := exec.Command(executable, arguments...) //nolint:gosec // native provider argv passes unchanged without a shell
	child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	child.Dir, child.Env = directory, env
	if err := child.Start(); err != nil {
		return fmt.Errorf("start %s: %w", executable, err)
	}
	stop := relayAgentSignals(child.Process)
	err := child.Wait()
	stop()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return statusError{code: agentExitCode(exited)}
	}
	if err != nil {
		return fmt.Errorf("wait for %s: %w", executable, err)
	}
	return nil
}

func preserveAgentTerminal(input io.Reader) func() {
	file, ok := input.(*os.File)
	if !ok {
		return func() {}
	}
	state, err := term.GetState(file.Fd())
	if err != nil {
		return func() {}
	}
	return func() { _ = term.Restore(file.Fd(), state) }
}

func relayAgentSignals(child *os.Process) func() {
	changes := make(chan os.Signal, 4)
	done := make(chan struct{})
	signal.Notify(changes, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT, os.Interrupt)
	go func() {
		for {
			select {
			case received := <-changes:
				forwardAgentSignal(child, received)
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(changes); close(done) }
}

func forwardAgentSignal(child *os.Process, received os.Signal) {
	// Terminal interrupts already reach the child's inherited foreground group.
	if received != os.Interrupt && received != syscall.SIGQUIT {
		_ = child.Signal(received)
	}
}

func agentExitCode(exited *exec.ExitError) int {
	if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exited.ExitCode()
}

func (a *application) agentBindCommand() *cobra.Command {
	return &cobra.Command{Use: "bind PROVIDER CONVERSATION_ID [-- ARG...]", Short: "Verify and save an exact native conversation", DisableFlagParsing: true,
		RunE: a.runAgentBind,
	}
}

func (a *application) runAgentBind(cmd *cobra.Command, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return cmd.Help()
	}
	if len(args) < 2 || len(args) > 2 && args[2] != "--" {
		return errors.New("use mesh agent bind PROVIDER CONVERSATION_ID [-- ARG...]")
	}
	provider, err := parseAgentProvider(args[0])
	if err != nil {
		return err
	}
	location, contained := worker.ContainingSessionWorker()
	if !contained {
		return errors.New("agent bind requires a containing Mesh terminal")
	}
	executable, err := resolveAgentExecutable(provider)
	if err != nil {
		return err
	}
	launch, err := inspectAgentLaunch(cmd.Context(), provider, executable, args[min(3, len(args)):])
	if err != nil {
		return err
	}
	recipe := agentresume.Recipe{Version: 1, Launch: launch, ConversationID: args[1]}
	if provider == agentresume.Codex {
		return bindCodexConversation(cmd, location, recipe)
	}
	return resumeAgentRecipe(cmd, location, recipe, true)
}

func bindCodexConversation(cmd *cobra.Command, location worker.SessionWorkerLocation, recipe agentresume.Recipe) error {
	event, err := agentresume.LookupConversation(cmd.Context(), recipe, clearAgentInvocationEnv(os.Environ()))
	if err != nil {
		return fmt.Errorf("verify explicit Codex conversation: %w", err)
	}
	recipe.Directory = event.Directory
	if err := agentresume.CheckAvailable(recipe); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), agentRequestTimeout)
	defer cancel()
	lease, err := beginAgentInvocation(ctx, location, recipe.Launch, recipe.ConversationID, true, true)
	if err != nil {
		return err
	}
	defer lease.connection.Close() //nolint:errcheck // binding is durable before success is printed
	response, err := daemonControlRequest(ctx, paths.Socket(location.Dir), protocol.Control{Type: protocol.TypeAgentEvent,
		SessionID: location.SessionID, AgentHostID: lease.hostID, AgentToken: lease.token, AgentProvider: recipe.Provider, AgentEvent: &event})
	if err != nil || response.Type != protocol.TypeAgentRegistered {
		return fmt.Errorf("save explicit Codex conversation: %w", agentResponseError(response, err))
	}
	verified, err := lease.finish()
	if err != nil || !verified {
		return fmt.Errorf("finish explicit Codex binding: %w", agentResponseError(protocol.Control{}, err))
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved exact Codex conversation %s in %s. Use mesh recover %s --agent to resume it.\n", recipe.ConversationID, recipe.Directory, location.SessionID)
	return err
}

func resumeAgentRecipe(cmd *cobra.Command, location worker.SessionWorkerLocation, recipe agentresume.Recipe, explicit bool) error {
	arguments, err := agentresume.ResumeCommand(recipe)
	if err != nil {
		return err
	}
	env, err := agentresume.ResumeEnv(recipe, clearAgentInvocationEnv(os.Environ()))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Opening the saved conversation. Mesh verifies recovery when the provider reports the expected conversation ID.")
	return runRegisteredAgent(cmd, location, recipe.Launch, arguments[1:], env, recipe.Directory, recipe.ConversationID, explicit)
}

func (a *application) agentResumeCommand() *cobra.Command {
	return &cobra.Command{Use: "agent-resume", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runPendingAgent(cmd)
	}}
}

func runPendingAgent(cmd *cobra.Command) error {
	location, contained := worker.ContainingSessionWorker()
	if !contained {
		return errors.New("agent-resume requires its reserved Mesh worker")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), agentRequestTimeout)
	defer cancel()
	metadata, err := awaitAgentMetadata(ctx, location.Dir)
	if err != nil {
		return fmt.Errorf("read reserved agent worker: %w", err)
	}
	host, err := identity.Load(filepath.Dir(filepath.Dir(location.Dir)))
	if err != nil {
		return fmt.Errorf("read agent recovery host: %w", err)
	}
	recipe, err := recovery.PendingAgent(filepath.Dir(location.Dir), host.ID, metadata.RecoveredFrom, location.SessionID)
	if err != nil {
		return err
	}
	if recipe == nil {
		return errors.New("reserved worker has no pending agent conversation")
	}
	if err := resumeAgentRecipe(cmd, location, *recipe, recipe.Explicit); err != nil {
		return openAgentRecoveryShell(cmd, location, metadata.RecoveredFrom, host.ID, recipe.Directory, err)
	}
	return nil
}

func awaitAgentMetadata(ctx context.Context, directory string) (worker.Meta, error) {
	for {
		metadata, err := worker.ReadMeta(directory)
		if !errors.Is(err, os.ErrNotExist) {
			return metadata, err
		}
		if err := waitAgentStartup(ctx); err != nil {
			return worker.Meta{}, err
		}
	}
}

func openAgentRecoveryShell(cmd *cobra.Command, location worker.SessionWorkerLocation, sourceID, hostID, directory string, failure error) error {
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return failure
	}
	previous, err := recovery.ReadSaved(filepath.Join(filepath.Dir(location.Dir), sourceID), hostID, sourceID, recovery.Record{})
	if err != nil {
		return errors.Join(failure, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Provider could not resume: %v\nPress Enter to open a shell in %s, or Ctrl+D to close.\n", failure, directory)
	if _, err := bufio.NewReader(input).ReadString('\n'); err != nil {
		return failure
	}
	if err := os.Chdir(directory); err != nil {
		return errors.Join(failure, fmt.Errorf("open saved agent directory: %w", err))
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Opening the saved shell. Conversation recovery remains unverified.")
	if err := syscall.Exec(previous.Shell, []string{previous.Shell}, clearAgentInvocationEnv(os.Environ())); err != nil { //nolint:gosec // saved host-local shell, selected by Enter; exec preserves its worker-validated PTY leader PID
		return errors.Join(failure, fmt.Errorf("open recovery shell: %w", err))
	}
	return nil
}

func reportAgentRecoveryStatus(output io.Writer, result recovery.Result) {
	if result.AgentStatus == "unverified" {
		_, _ = fmt.Fprintln(output, "Conversation recovery is unverified while the provider starts. This terminal remains usable; retry reconnects to the same replacement.")
	}
}
