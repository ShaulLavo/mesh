package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/worker"
)

const (
	defaultCatalogTimeout = 750 * time.Millisecond
	remoteConnectTimeout  = 5 * time.Second
	remoteCreateTimeout   = 40 * time.Second
	localQueryTimeout     = 500 * time.Millisecond
)

// AddRequest is the CLI-owned input to the SSH bootstrap adapter.
type AddRequest struct {
	Target               string
	Alias                string
	TailscaleAuthKeyFile string
	// IdentityFile names the key to adopt with, for a host the ssh config
	// says nothing about.
	IdentityFile string
	Yes          bool
}

// BootstrapResult separates the durable address-book entry from metadata about
// the bootstrap operation that produced it.
type BootstrapResult struct {
	Host              HostRecord
	AlreadyConfigured bool
}

// BootstrapFunc installs a host and returns its verified address-book record.
type BootstrapFunc func(context.Context, AddRequest) (BootstrapResult, error)

// WakeFunc wakes one adopted host through a configured power controller.
type WakeFunc func(context.Context, HostRecord) error

// ContainmentFunc returns the exact immediate-to-outer Mesh sessions whose
// terminal screens receive this process's output.
type ContainmentFunc func(context.Context) []protocol.SessionIdentity

// PickerInput contains the catalog and the optional live readers used by the
// interactive picker.
type PickerInput struct {
	Hosts     []HostSessions
	LoadHosts func(context.Context) ([]HostSessions, error)
	Inspect   PickerInspectFunc
	Refresh   PickerRefreshFunc
	Action    PickerSessionActionFunc
	// ContainingSessions are the exact terminal screens that receive this
	// picker's output. Each available snapshot was captured before the picker
	// wrote its first frame.
	ContainingSessions []PickerContainingSession
	// OpenHostAlias keeps the session browser on this host after waking it and
	// refreshing the catalog. An empty alias opens the host list.
	OpenHostAlias string
}

// PickerContainingSession pairs one exact mirrored terminal with the last
// screen observed before the picker opened. A nil Snapshot means that Mesh
// proved the identity but could not capture that screen safely.
type PickerContainingSession struct {
	Identity   protocol.SessionIdentity
	Snapshot   *SessionInspection
	ReceivedAt time.Time
}

// PickerHostSnapshot is the current data rendered for one open host. A nil
// Services value keeps the picker’s last service rows but marks that view stale
// when that side of the refresh could not be read.
type PickerHostSnapshot struct {
	Sessions HostSessions
	Services *PickerServiceCatalog
}

// PickerServiceCatalog is one host’s live or cached served websites.
type PickerServiceCatalog struct {
	Rows  []ServiceCatalogRow
	Stale bool
}

// PickerRefreshFunc reads the current data for one open host.
type PickerRefreshFunc func(context.Context, string) (PickerHostSnapshot, error)

// PickerSessionAction identifies an in-panel mutation of one remote session.
type PickerSessionAction uint8

const (
	PickerKillSession PickerSessionAction = iota + 1
	PickerRemoveSession
)

// PickerSessionActionRequest names one session mutation without exposing host
// connection details to the picker.
type PickerSessionActionRequest struct {
	HostAlias string
	SessionID string
	Action    PickerSessionAction
}

// PickerSessionActionFunc performs a mutation while the picker stays open.
type PickerSessionActionFunc func(context.Context, PickerSessionActionRequest) error

// PickerInspectRequest identifies one highlighted session and bounds the
// preview it may return. The host alias is resolved against the same catalog
// that the picker displays, so the UI never handles connection details.
type PickerInspectRequest struct {
	HostAlias   string
	SessionID   string
	PreviewCols int
	PreviewRows int
}

// SessionDirectorySource says how the host observed a live working directory.
type SessionDirectorySource uint8

const (
	SessionDirectoryUnknown SessionDirectorySource = iota
	SessionDirectoryProcess
	SessionDirectoryTerminal
)

// SessionInspection is the live, non-durable view of one selected session.
// Launch command and directory stay in SessionInfo because they describe a
// different fact and remain useful while a host is offline.
type SessionInspection struct {
	ObservedAt        time.Time
	CurrentDirectory  string
	DirectorySource   SessionDirectorySource
	ForegroundCommand string
	TerminalTitle     string
	LastOutputAt      *time.Time
	Attached          bool
	Preview           []string
	StyledPreview     []protocol.PreviewLine
	Nested            []protocol.SessionIdentity
}

// PickerInspectFunc reads one live session without attaching to it.
type PickerInspectFunc func(context.Context, PickerInspectRequest) (SessionInspection, error)

// PickerSelection tells the CLI which normal action to run after T09 exits.
type PickerSelection struct {
	HostAlias      string
	SessionID      string
	New            bool
	Wake           bool
	Relaunch       bool
	RecoveryAction recovery.Action
	TakeOver       bool
}

// WindowInput is the local catalog available before any network discovery.
type WindowInput struct {
	Sessions    []protocol.SessionInfo
	Inspect     PickerInspectFunc
	Action      PickerSessionActionFunc
	HostAlias   string
	HostID      string
	HostAliases map[string]string
}

type WindowSelection struct {
	SessionID      string
	New            bool
	FullPicker     bool
	Relaunch       bool
	RecoveryAction recovery.Action
}

type WindowPickerFunc func(context.Context, WindowInput) (WindowSelection, error)

// PickerFunc owns only selection UI. The CLI still owns create and attach.
type PickerFunc func(context.Context, PickerInput) (PickerSelection, error)

// Dependencies are the replaceable product edges used by later tasks and tests.
type Dependencies struct {
	SSHSessionHandler     sshd.SessionHandlerFactory
	Bootstrap             BootstrapFunc
	Wake                  WakeFunc
	Picker                PickerFunc
	WindowPicker          WindowPickerFunc
	ReconcilePrivateNames PrivateNamesFunc
	DialHost              HostDialer
	DialControl           HostDialer
	ConfirmPublic         ConfirmPublicFunc
	Containment           ContainmentFunc
	Now                   func() time.Time
	Stdin                 *os.File
	Stdout                *os.File
	Stderr                *os.File
}

type application struct {
	dependencies Dependencies
}

// NewCommand builds the complete non-picker CLI. Fang decorates and executes
// the returned Cobra command in cmd/mesh.
func NewCommand(dependencies Dependencies) *cobra.Command {
	if dependencies.DialHost == nil {
		dependencies.DialHost = dialHost
	}
	if dependencies.DialControl == nil {
		dependencies.DialControl = dialControlHost
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.Containment == nil {
		dependencies.Containment = discoverContainingSessions
	}
	if dependencies.ReconcilePrivateNames == nil {
		dependencies.ReconcilePrivateNames = reconcilePrivateNames
	}
	if dependencies.Stdin == nil {
		dependencies.Stdin = os.Stdin
	}
	if dependencies.Stdout == nil {
		dependencies.Stdout = os.Stdout
	}
	if dependencies.Stderr == nil {
		dependencies.Stderr = os.Stderr
	}
	if dependencies.ConfirmPublic == nil {
		dependencies.ConfirmPublic = terminalPublicConfirmation(dependencies.Stdin, dependencies.Stderr)
	}
	app := &application{dependencies: dependencies}

	var (
		resume    bool
		detachKey string
		raw       bool
		window    bool
		take      bool
	)
	root := &cobra.Command{
		Use:   "mesh [host|session] [-- command...]",
		Short: "Direct, resumable terminal sessions across your machines",
		Long: "Sessions live on the host that runs them, not on the connection.\n" +
			"Close your laptop, lose wifi, or quit mesh, and the command keeps\n" +
			"running. Reattach from anywhere on your tailnet.\n\n" +
			"A bare name is a host or a session, so the commands you use most are\n" +
			"not subcommands at all.",
		// The everyday forms are the root command itself, so nothing below
		// lists them. Without this a reader never learns `mesh pc` exists.
		// Fang parses each line as a command and annotates with "# " comments,
		// so prose trailing a command is swallowed rather than printed.
		Example: "# Pick a host and session\n" +
			"mesh\n" +
			"# Start a new session on pc\n" +
			"mesh pc\n" +
			"# Reattach to the newest session on pc\n" +
			"mesh pc -r\n" +
			"# Attach to a session by its id\n" +
			"mesh 7K3D\n" +
			"# Run one command there, then come back\n" +
			"mesh pc -- htop\n" +
			"# Detach and leave it running: ctrl+]",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if window {
				if len(args) != 0 || resume {
					return errors.New("--window cannot be combined with a host, session, command, or --resume")
				}
				return app.runWindow(cmd, take, detachKey, raw)
			}
			if take {
				return errors.New("--take requires --window")
			}
			return app.runRoot(cmd, args, resume, detachKey, raw)
		},
	}
	root.SetIn(dependencies.Stdin)
	root.SetOut(dependencies.Stdout)
	root.SetErr(dependencies.Stderr)
	root.Flags().BoolVarP(&resume, "resume", "r", false, "resume the newest active session on the host")
	root.Flags().StringVar(&detachKey, "detach-key", "", "key that detaches, such as ctrl+] or none")
	root.Flags().BoolVar(&raw, "raw", false, "pass every input byte through without a detach key")
	root.Flags().BoolVar(&window, "window", false, "open a terminal window with a local persistent session")
	root.Flags().BoolVar(&take, "take", false, "resume the newest detached session without a prompt; requires --window")
	root.PersistentFlags().String("leave-key", "ctrl+^", "key that leaves all nested sessions, or none")

	// Grouped so the commands used daily are not sorted in among the ones
	// touched once per machine.
	root.AddGroup(
		&cobra.Group{ID: groupSessions, Title: "Sessions"},
		&cobra.Group{ID: groupHosts, Title: "Hosts"},
		&cobra.Group{ID: groupServing, Title: "Serving"},
		&cobra.Group{ID: groupSetup, Title: "Setup"},
	)
	root.AddCommand(
		inGroup(groupSessions, app.listCommand(), app.attachCommand(), app.localCommand(),
			app.logsCommand(), app.killCommand(), app.signalCommand(), app.removeCommand(), app.recoverCommand(), app.recoveryCommandCommand())...,
	)
	root.AddCommand(inGroup(groupHosts, app.addCommand(), app.renameCommand(), app.wakeCommand())...)
	root.AddCommand(inGroup(groupServing, app.serveCommand(), app.unserveCommand())...)
	root.AddCommand(inGroup(groupSetup, daemonWithInstall(app), app.privateNamesCommand(), app.shellInitCommand())...)
	root.AddCommand(app.workerCommand(), app.shellUpdateCommand())
	return root
}

type statusError struct{ code int }

func (e statusError) Error() string { return fmt.Sprintf("process exited with status %d", e.code) }

// StatusCode returns a process exit status carried through Cobra without
// asking Fang to print it as a CLI failure.
func StatusCode(err error) (int, bool) {
	var status statusError
	if !errors.As(err, &status) {
		return 0, false
	}
	return status.code, true
}

func (a *application) runRoot(cmd *cobra.Command, args []string, resume bool, detachKey string, raw bool) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if resume {
			return errors.New("--resume needs a host alias")
		}
		return a.runPicker(cmd, hosts, detachKey, raw)
	}
	target, err := ResolveArgument(args[0], hosts)
	if err != nil {
		return err
	}
	if target.Host != nil {
		command := args[1:]
		if resume && len(command) > 0 {
			return errors.New("--resume cannot be combined with a command")
		}
		return a.runHost(cmd, *target.Host, resume, command, detachKey, raw)
	}
	if resume {
		return errors.New("--resume needs a host alias, not a session ID")
	}
	if len(args) != 1 {
		return fmt.Errorf("a session ID does not accept a command; use a host alias before --")
	}
	resolved, err := a.resolveSession(cmd.Context(), hosts, target.SessionID)
	if err != nil {
		return err
	}
	return a.attachResolved(cmd, resolved, detachKey, raw, nil)
}

func (a *application) runPicker(cmd *cobra.Command, hosts []HostRecord, detachKey string, raw bool) error {
	return a.runPickerOpen(cmd, hosts, detachKey, raw, "")
}

func (a *application) runPickerOpen(cmd *cobra.Command, hosts []HostRecord, detachKey string, raw bool, openHostAlias string) error {
	if a.dependencies.Picker == nil {
		return errors.New("the interactive picker is not installed yet; run mesh <host> or mesh <session-id>")
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	refreshSessions := func(ctx context.Context, alias string) (HostSessions, error) {
		if alias == localHostAlias {
			return localPickerCatalog()
		}
		host, err := hostWithAlias(hosts, alias)
		if err != nil {
			return HostSessions{}, err
		}
		return a.refreshPickerSessions(ctx, host, cache)
	}
	refreshHost := func(ctx context.Context, alias string) (PickerHostSnapshot, error) {
		if alias == localHostAlias {
			local, err := localPickerCatalog()
			return PickerHostSnapshot{Sessions: local}, err
		}
		host, err := hostWithAlias(hosts, alias)
		if err != nil {
			return PickerHostSnapshot{}, err
		}
		return a.refreshPickerHost(ctx, host, cache)
	}
	containingIdentities := a.dependencies.Containment(cmd.Context())
	if err := protocol.ValidateContainingSessions(containingIdentities); err != nil {
		return fmt.Errorf("read terminal containment: %w", err)
	}
	containingIdentities = protocol.CloneSessionIdentities(containingIdentities)
	var containingSessions []PickerContainingSession
	containmentCaptured := false
	for {
		local, err := localPickerCatalog()
		if err != nil {
			return err
		}
		catalog := []HostSessions{local}
		for _, host := range hosts {
			cached, cacheErr := cache.Load(cmd.Context(), host)
			catalog = append(catalog, HostSessions{Host: host, Sessions: cached, Stale: true, CacheErr: cacheErr})
		}
		if !containmentCaptured {
			containmentCaptured = true
			containingSessions = a.capturePickerContainingSessions(cmd.Context(), catalog, containingIdentities)
		}
		pickerContext, cancelPicker := context.WithCancel(cmd.Context())
		pickerOperations := newPickerOperationGate()
		selection, err := a.dependencies.Picker(pickerContext, PickerInput{
			Hosts: catalog,
			LoadHosts: func(ctx context.Context) ([]HostSessions, error) {
				if !pickerOperations.begin(ctx) {
					return nil, context.Canceled
				}
				defer pickerOperations.done()
				return CollectHostSessions(ctx, hosts, defaultCatalogTimeout, a.queryHost, cache)
			},
			OpenHostAlias:      openHostAlias,
			ContainingSessions: clonePickerContainingSessions(containingSessions),
			Refresh: func(ctx context.Context, alias string) (PickerHostSnapshot, error) {
				if !pickerOperations.begin(ctx) {
					return PickerHostSnapshot{}, context.Canceled
				}
				defer pickerOperations.done()
				return refreshHost(ctx, alias)
			},
			Action: func(ctx context.Context, request PickerSessionActionRequest) error {
				if !pickerOperations.begin(ctx) {
					return context.Canceled
				}
				defer pickerOperations.done()
				return a.pickerSessionAction(ctx, hosts, request)
			},
			Inspect: func(ctx context.Context, request PickerInspectRequest) (SessionInspection, error) {
				if !pickerOperations.begin(ctx) {
					return SessionInspection{}, context.Canceled
				}
				defer pickerOperations.done()
				if request.HostAlias == localHostAlias {
					return inspectLocalSession(ctx, request)
				}
				host, err := hostWithAlias(hosts, request.HostAlias)
				if err != nil {
					return SessionInspection{}, err
				}
				return inspectRemoteSession(ctx, host, a.dependencies.DialControl, request.SessionID, request.PreviewCols, request.PreviewRows)
			},
		})
		cancelPicker()
		pickerOperations.stopAndWait()
		if err != nil {
			return err
		}
		if err := validatePickerSelection(selection); err != nil {
			return err
		}
		if selection.SessionID != "" {
			if selection.HostAlias == localHostAlias {
				current, err := Find(selection.SessionID)
				if err != nil {
					return err
				}
				if selection.Relaunch {
					return a.relaunchSession(cmd, resolvedSession{local: &current}, detachKey, raw, selection.RecoveryAction)
				}
				return a.attachPickerSession(cmd, resolvedSession{local: &current}, selection.TakeOver, detachKey, raw, containingIdentities)
			}
			selectionCatalog := catalog
			if selection.HostAlias != "" {
				refreshContext, cancel := context.WithTimeout(cmd.Context(), 2*defaultCatalogTimeout)
				refreshed, refreshErr := refreshSessions(refreshContext, selection.HostAlias)
				cancel()
				if refreshErr != nil {
					return refreshErr
				}
				selectionCatalog = []HostSessions{refreshed}
			}
			resolved, err := findCatalogSession(selectionCatalog, selection.SessionID, selection.HostAlias)
			if err != nil {
				return err
			}
			if selection.Relaunch {
				return a.relaunchSession(cmd, resolved, detachKey, raw, selection.RecoveryAction)
			}
			return a.attachPickerSession(cmd, resolved, selection.TakeOver, detachKey, raw, containingIdentities)
		}
		if selection.HostAlias == "" {
			return nil
		}
		if !selection.New && !selection.Wake {
			refreshed, err := refreshSessions(cmd.Context(), selection.HostAlias)
			if err != nil {
				return err
			}
			for _, row := range windowSessionRows(refreshed.Sessions) {
				if row.State != worker.StateDetached {
					continue
				}
				resolved := resolvedSession{host: &refreshed.Host, remote: row}
				if refreshed.Local {
					current, err := Find(row.ID)
					if err != nil {
						return err
					}
					resolved = resolvedSession{local: &current}
				}
				err := a.attachPickerSession(cmd, resolved, false, detachKey, raw, containingIdentities)
				if errors.Is(err, ErrSessionAttached) || errors.Is(err, ErrSessionUnavailable) {
					continue
				}
				return err
			}
			return fmt.Errorf("%s has no detached sessions; select an in-use session to take over", selection.HostAlias)
		}
		if selection.HostAlias == localHostAlias {
			return a.startWindowSession(cmd, detachKey, raw)
		}
		host, err := hostWithAlias(hosts, selection.HostAlias)
		if err != nil {
			return err
		}
		if selection.Wake {
			openHostAlias = selection.HostAlias
			if err := a.wakeHost(cmd.Context(), host, cmd.ErrOrStderr()); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "woke %s; refreshing hosts\n", host.Alias); err != nil {
				return err
			}
			continue
		}
		return a.runHostWithContainment(
			cmd,
			host,
			!selection.New,
			nil,
			detachKey,
			raw,
			func(context.Context) []protocol.SessionIdentity {
				return protocol.CloneSessionIdentities(containingIdentities)
			},
		)
	}
}

const pickerContainingSessionCaptureTimeout = 1500 * time.Millisecond

func (a *application) capturePickerContainingSessions(
	parent context.Context,
	catalog []HostSessions,
	identities []protocol.SessionIdentity,
) []PickerContainingSession {
	captured := make([]PickerContainingSession, len(identities))
	for index, identity := range identities {
		captured[index].Identity = identity
	}
	if len(identities) == 0 {
		return captured
	}

	hosts := make(map[string]HostRecord, len(catalog))
	for _, current := range catalog {
		if !current.Local {
			continue
		}
		hosts[current.Host.ID] = current.Host
	}
	type captureResult struct {
		index int
		value SessionInspection
		err   error
	}
	results := make(chan captureResult, len(identities))
	ctx, cancel := context.WithTimeout(parent, pickerContainingSessionCaptureTimeout)
	defer cancel()
	pending := 0
	for index, identity := range identities {
		host, exists := hosts[identity.HostID]
		if !exists {
			continue
		}
		pending++
		go func() {
			value, err := inspectLocalSession(ctx, PickerInspectRequest{
				HostAlias: host.Alias, SessionID: identity.SessionID,
				PreviewCols: protocol.MaxInspectionPreviewCols, PreviewRows: protocol.MaxInspectionPreviewRows,
			})
			results <- captureResult{index: index, value: value, err: err}
		}()
	}
	for range pending {
		select {
		case result := <-results:
			if result.err != nil {
				continue
			}
			value := copySessionInspection(result.value)
			captured[result.index].Snapshot = &value
			captured[result.index].ReceivedAt = a.dependencies.Now()
		case <-ctx.Done():
			return captured
		}
	}
	return captured
}

func clonePickerContainingSessions(source []PickerContainingSession) []PickerContainingSession {
	cloned := make([]PickerContainingSession, len(source))
	for index, current := range source {
		cloned[index] = current
		if current.Snapshot != nil {
			value := copySessionInspection(*current.Snapshot)
			cloned[index].Snapshot = &value
		}
	}
	return cloned
}

func copySessionInspection(source SessionInspection) SessionInspection {
	copy := source
	copy.Nested = protocol.CloneSessionIdentities(source.Nested)
	copy.Preview = append([]string(nil), source.Preview...)
	copy.StyledPreview = copyPreviewLines(source.StyledPreview)
	if source.LastOutputAt != nil {
		lastOutputAt := *source.LastOutputAt
		copy.LastOutputAt = &lastOutputAt
	}
	return copy
}

func copyPreviewLines(source []protocol.PreviewLine) []protocol.PreviewLine {
	if source == nil {
		return nil
	}
	copy := make([]protocol.PreviewLine, len(source))
	for row, line := range source {
		copy[row].Runs = append([]protocol.PreviewRun(nil), line.Runs...)
	}
	return copy
}

func validatePickerSelection(selection PickerSelection) error {
	if selection.SessionID != "" && (selection.New || selection.Wake) {
		return errors.New("picker selection cannot combine a session with new or wake")
	}
	if selection.New && selection.Wake {
		return errors.New("picker selection cannot combine new and wake")
	}
	if (selection.New || selection.Wake) && selection.HostAlias == "" {
		return errors.New("picker new and wake selections require a host alias")
	}
	return nil
}

func (a *application) runHost(cmd *cobra.Command, host HostRecord, resume bool, command []string, detachKey string, raw bool) error {
	return a.runHostWithContainment(
		cmd,
		host,
		resume,
		command,
		detachKey,
		raw,
		a.dependencies.Containment,
	)
}

func (a *application) runHostWithContainment(
	cmd *cobra.Command,
	host HostRecord,
	resume bool,
	command []string,
	detachKey string,
	raw bool,
	containment ContainmentFunc,
) error {
	if resume {
		ctx, cancel := context.WithTimeout(cmd.Context(), wakeIntentTimeout)
		rows, err := listRemoteHost(ctx, host, a.intentDialer(cmd.ErrOrStderr()))
		cancel()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.State == string(storage.StateRunning) || row.State == string(storage.StateDetached) {
				containingSessions := containment(cmd.Context())
				return a.attachResolvedWithContainment(
					cmd,
					resolvedSession{host: &host, remote: row},
					detachKey,
					raw,
					nil,
					containingSessions,
				)
			}
		}
		return fmt.Errorf("host %s has no active sessions", host.Alias)
	}
	// No command means the host's own shell. Sending defaultShell() here would
	// name a path on this machine: /bin/zsh on a Mac does not exist on an Arch
	// host, whose shell is /usr/bin/bash.
	cols, rows := terminalSize(a.dependencies.Stdout)
	ctx, cancel := context.WithTimeout(cmd.Context(), remoteCreateTimeout+wakeIntentTimeout)
	id, err := createRemoteSession(ctx, host, a.intentDialer(cmd.ErrOrStderr()), command, cols, rows)
	cancel()
	if err != nil {
		return err
	}
	// "session X on pc" reads like one was found. `mesh pc` always creates,
	// and `mesh pc -r` is the one that attaches to an existing session.
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "new session %s on %s\n", id, host.Alias); err != nil {
		return err
	}
	initial := uint64(0)
	containingSessions := containment(cmd.Context())
	return a.attachResolvedWithContainment(
		cmd,
		resolvedSession{host: &host, remote: protocol.SessionInfo{ID: id, HostID: host.ID, State: string(storage.StateRunning)}},
		detachKey,
		raw,
		&initial,
		containingSessions,
	)
}

type resolvedSession struct {
	host       *HostRecord
	remote     protocol.SessionInfo
	local      *Session
	stale      bool
	ifDetached bool
}

// resolveSession looks for a session on this machine and across the address
// book. This host is never in its own address book, so gating the local lookup
// on an empty one made every local session invisible to logs, kill and sig the
// moment the first remote host was adopted — while the worker kept running.
func (a *application) resolveSession(ctx context.Context, hosts []HostRecord, id string) (resolvedSession, error) {
	local, localErr := Find(id)
	switch {
	case localErr == nil:
	case errors.Is(localErr, ErrNoLocalSession):
		local = Session{}
	default:
		// Reading the local session directory failed for a reason that is not
		// "absent". Surfacing it beats reporting the session missing.
		return resolvedSession{}, localErr
	}
	foundLocally := localErr == nil

	if len(hosts) == 0 {
		if !foundLocally {
			return resolvedSession{}, localErr
		}
		return resolvedSession{local: &local}, nil
	}

	cache, err := OpenCatalogCache(ctx)
	if err != nil {
		return resolvedSession{}, err
	}
	defer cache.Close() //nolint:errcheck // lookup result takes precedence
	catalog, err := CollectHostSessions(ctx, hosts, defaultCatalogTimeout, a.queryHost, cache)
	if err != nil {
		return resolvedSession{}, err
	}
	remote, remoteErr := findCatalogSession(catalog, id, "")
	switch {
	case foundLocally && remoteErr == nil:
		return resolvedSession{}, fmt.Errorf("session %s is both on this host and on %s; name the host to choose", strings.ToUpper(id), remote.host.Alias)
	case foundLocally:
		return resolvedSession{local: &local}, nil
	default:
		return remote, remoteErr
	}
}

func findCatalogSession(catalog []HostSessions, id, hostAlias string) (resolvedSession, error) {
	var matches []resolvedSession
	for _, result := range catalog {
		if hostAlias != "" && !strings.EqualFold(result.Host.Alias, hostAlias) {
			continue
		}
		for _, row := range result.Sessions {
			if strings.EqualFold(row.ID, id) {
				host := result.Host
				matches = append(matches, resolvedSession{host: &host, remote: row, stale: result.Stale})
			}
		}
	}
	switch len(matches) {
	case 0:
		return resolvedSession{}, fmt.Errorf("no session %s on any known host", strings.ToUpper(id))
	case 1:
		return matches[0], nil
	default:
		aliases := make([]string, len(matches))
		for i, match := range matches {
			aliases[i] = match.host.Alias
		}
		sort.Strings(aliases)
		return resolvedSession{}, fmt.Errorf("session %s exists on multiple hosts: %s", strings.ToUpper(id), strings.Join(aliases, ", "))
	}
}

func (a *application) attachResolved(cmd *cobra.Command, resolved resolvedSession, detachKey string, raw bool, lastSeq *uint64) error {
	return a.attachResolvedWithContainment(
		cmd,
		resolved,
		detachKey,
		raw,
		lastSeq,
		a.dependencies.Containment(cmd.Context()),
	)
}

func (a *application) attachResolvedWithContainment(
	cmd *cobra.Command,
	resolved resolvedSession,
	detachKey string,
	raw bool,
	lastSeq *uint64,
	containingSessions []protocol.SessionIdentity,
) error {
	if len(containingSessions) > 0 {
		target, err := resolvedTargetIdentity(resolved)
		if err != nil {
			return fmt.Errorf("resolve attach target containment: %w", err)
		}
		if err := rejectContainingTarget(target, containingSessions); err != nil {
			return err
		}
	}
	options, err := a.attachmentOptions(cmd, detachKey, raw)
	if err != nil {
		return err
	}
	options.LastSeq = lastSeq
	options.ContainingSessions = containingSessions
	options.IfDetached = resolved.ifDetached
	var display string
	if resolved.local != nil {
		if !resolved.local.Alive {
			return fmt.Errorf("session %s is %s", resolved.local.ID, resolved.local.State())
		}
		options.SocketPath = paths.Socket(resolved.local.Dir)
		options.SessionID = resolved.local.ID
		display = resolved.local.ID
	} else {
		if resolved.remote.State != string(storage.StateRunning) && resolved.remote.State != string(storage.StateDetached) {
			return fmt.Errorf("session %s on %s is %s", resolved.remote.ID, resolved.host.Alias, resolved.remote.State)
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), wakeIntentTimeout)
		conn, err := openVerifiedHost(ctx, *resolved.host, a.intentDialer(cmd.ErrOrStderr()))
		cancel()
		if err != nil {
			return err
		}
		options.Conn = conn
		options.HostID = resolved.host.ID
		options.SessionID = resolved.remote.ID
		display = resolved.remote.ID + " on " + resolved.host.Alias
	}
	result, err := Attach(options)
	if err != nil {
		return err
	}
	switch {
	case result.Exited:
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "\r\nsession %s exited (%d)\r\n", display, result.ExitCode); err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return statusError{code: result.ExitCode}
		}
	case result.Detached:
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "\r\ndetached from %s, still running\r\n", display)
	default:
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "\r\ndisconnected from %s\r\n", display)
	}
	return err
}

func (a *application) queryHost(ctx context.Context, host HostRecord) ([]protocol.SessionInfo, error) {
	return listRemoteHost(ctx, host, a.dependencies.DialHost)
}

func (a *application) addCommand() *cobra.Command {
	var (
		alias                string
		tailscaleAuthKeyFile string
		yes                  bool
		identityFile         string
	)
	command := &cobra.Command{
		Use:   "add [user@]host",
		Short: "Install Mesh on an SSH-reachable host",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.addTargetUsage(cmd)
			}
			selected := alias
			if selected == "" {
				selected = aliasFromTarget(args[0])
			}
			selected, err := ValidateHostAlias(selected)
			if err != nil {
				return err
			}
			if a.dependencies.Bootstrap == nil {
				return errors.New("SSH bootstrap support is unavailable in this build")
			}
			result, err := a.dependencies.Bootstrap(cmd.Context(), AddRequest{
				Target: args[0], Alias: selected, IdentityFile: identityFile,
				TailscaleAuthKeyFile: tailscaleAuthKeyFile,
				Yes:                  yes,
			})
			if err != nil {
				return err
			}
			record := result.Host
			record.Alias = selected
			if current := existingAliasFor(record.ID, selected); current != "" {
				rename, err := a.confirmRename(cmd, current, selected)
				if err != nil {
					return err
				}
				if !rename {
					record.Alias = current
					selected = current
				}
			}
			if err := SaveHost(record); err != nil {
				return fmt.Errorf("save host %s: %w", selected, err)
			}
			path, _ := ConfigPath()
			// AlreadyConfigured means nothing on the remote changed, not that
			// the host was in the address book. "already configured" read as
			// the second and left people wondering what the run had just done.
			if result.AlreadyConfigured {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s was already up to date (%s)\nhost config: %s\n", selected, record.ID, path)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "added %s (%s)\nhost config: %s\n", selected, record.ID, path)
			}
			return err
		},
	}
	command.Flags().StringVar(&alias, "alias", "", "local name for the host")
	command.Flags().StringVar(&tailscaleAuthKeyFile, "tailscale-auth-key-file", "", "read a Tailscale auth key from this local file")
	command.Flags().BoolVar(&yes, "yes", false, "approve remote Tailscale installation and user lingering changes")
	command.Flags().StringVar(&identityFile, "identity-file", "", "SSH private key to adopt with, when ~/.ssh/config names none")
	return command
}

func (a *application) wakeCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "wake host",
		Short: "Wake a configured host",
		Args:  exactArgs(1, "the name of a host you have added", "mesh wake pc        (mesh ls lists your hosts)"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := LoadHosts()
			if err != nil {
				return err
			}
			host, err := hostWithAlias(hosts, args[0])
			if err != nil {
				return err
			}
			if err := a.wakeHost(cmd.Context(), host, cmd.ErrOrStderr()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "woke %s\n", host.Alias)
			return err
		},
	}
	command.AddCommand(a.configureWakeCommand(true), a.configureWakeCommand(false))
	return command
}

func hostWithAlias(hosts []HostRecord, alias string) (HostRecord, error) {
	for _, host := range hosts {
		if strings.EqualFold(host.Alias, alias) {
			return host, nil
		}
	}
	return HostRecord{}, fmt.Errorf("unknown host alias %q", alias)
}

func aliasFromTarget(target string) string {
	host := target
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if strings.HasPrefix(host, "[") {
		if closeBracket := strings.IndexByte(host, ']'); closeBracket > 0 {
			host = host[1:closeBracket]
		}
	} else if colon := strings.LastIndexByte(host, ':'); colon > 0 && strings.Count(host, ":") == 1 {
		host = host[:colon]
	}
	if dot := strings.IndexByte(host, '.'); dot > 0 {
		host = host[:dot]
	}
	return host
}

func (a *application) listCommand() *cobra.Command {
	var (
		viaDaemon bool
		timeout   time.Duration
	)
	command := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sessions across known hosts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runList(cmd, viaDaemon, timeout)
		},
	}
	command.Flags().BoolVar(&viaDaemon, "daemon", false, "read only the local daemon catalog")
	command.Flags().DurationVar(&timeout, "timeout", defaultCatalogTimeout, "maximum wait for the host fan-out")
	return command
}

func (a *application) runList(cmd *cobra.Command, viaDaemon bool, timeout time.Duration) error {
	if viaDaemon {
		stateDir, err := paths.StateDir()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), localQueryTimeout)
		rows, err := ListViaDaemon(ctx, meshdaemon.SocketPath(stateDir))
		cancel()
		if err != nil {
			return err
		}
		return writeProtocolSessions(cmd.OutOrStdout(), a.dependencies.Now(), []HostSessions{{Host: HostRecord{Alias: "local"}, Sessions: rows}})
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		rows, err := List()
		if err != nil {
			return err
		}
		return writeLocalSessions(cmd.OutOrStdout(), a.dependencies.Now(), rows)
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	results, err := CollectHostSessions(cmd.Context(), hosts, timeout, a.queryHost, cache)
	if err != nil {
		return err
	}
	// This host is never in its own address book, so its sessions have to be
	// added deliberately. Without this, adopting one remote host hid every
	// local session from `mesh ls` while its worker kept running.
	if localRows, err := localSessionRows(); err != nil {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "this host: local sessions unavailable: %s\n", safeRemoteText(err.Error())); err != nil {
			return err
		}
	} else if len(localRows) > 0 {
		results = append([]HostSessions{{Host: HostRecord{Alias: localHostAlias}, Sessions: localRows}}, results...)
	}
	if err := writeProtocolSessions(cmd.OutOrStdout(), a.dependencies.Now(), results); err != nil {
		return err
	}
	for _, result := range results {
		if result.Err != nil {
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s: unavailable: %s; cached rows are stale\n", result.Host.Alias, safeRemoteText(result.Err.Error())); err != nil {
				return err
			}
		} else if result.CacheErr != nil {
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s: live results could not be cached: %s\n", result.Host.Alias, safeRemoteText(result.CacheErr.Error())); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeLocalSessions(output io.Writer, now time.Time, sessions []Session) error {
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(output, "no sessions on this host")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tSTATE\tAGE\tSTARTED IN\tCOMMAND"); err != nil {
		return err
	}
	for _, current := range sessions {
		if _, err := fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\t%s\n",
			current.ID,
			current.State(),
			ageAt(now, current.CreatedAt),
			sessionLaunchDirectory(current.Cwd),
			SafeTerminalText(strings.Join(current.Command, " ")),
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeProtocolSessions(output io.Writer, now time.Time, hosts []HostSessions) error {
	type row struct {
		host    string
		session protocol.SessionInfo
		stale   bool
	}
	var rows []row
	for _, result := range hosts {
		for _, current := range result.Sessions {
			rows = append(rows, row{host: result.Host.Alias, session: current, stale: result.Stale})
		}
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(output, "no sessions on known hosts")
		return err
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].session.CreatedAt.Equal(rows[j].session.CreatedAt) {
			return rows[i].session.CreatedAt.After(rows[j].session.CreatedAt)
		}
		if rows[i].host != rows[j].host {
			return rows[i].host < rows[j].host
		}
		return rows[i].session.ID < rows[j].session.ID
	})
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "HOST\tID\tSTATE\tAGE\tSTARTED IN\tCOMMAND\tCACHE"); err != nil {
		return err
	}
	for _, current := range rows {
		cache := "-"
		if current.stale {
			cache = "stale"
		}
		if _, err := fmt.Fprintf(
			table,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			current.host,
			current.session.ID,
			current.session.State,
			ageAt(now, current.session.CreatedAt),
			sessionLaunchDirectory(current.session.Cwd),
			SafeTerminalText(strings.Join(current.session.Command, " ")),
			cache,
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

func sessionLaunchDirectory(cwd string) string {
	if cwd == "" {
		return "-"
	}
	return SafeTerminalText(cwd)
}

func (a *application) localCommand() *cobra.Command {
	var (
		resume        bool
		requireDaemon bool
		detachKey     string
		raw           bool
	)
	command := &cobra.Command{
		Use:   "local [-- command...]",
		Short: "Start or resume a session on this host",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runLocal(cmd, args, resume, requireDaemon, detachKey, raw)
		},
	}
	command.Flags().BoolVarP(&resume, "resume", "r", false, "resume the latest live session")
	command.Flags().BoolVar(&requireDaemon, "daemon", false, "require the local daemon")
	command.Flags().StringVar(&detachKey, "detach-key", "", "key that detaches, such as ctrl+] or none")
	command.Flags().BoolVar(&raw, "raw", false, "pass every input byte through")
	return command
}

func (a *application) runLocal(cmd *cobra.Command, command []string, resume bool, requireDaemon bool, detachKey string, raw bool) error {
	var (
		current    Session
		socketPath string
		lastSeq    *uint64
		err        error
	)
	if resume {
		if len(command) > 0 {
			return errors.New("--resume cannot be combined with a command")
		}
		current, err = Latest()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "resuming %s\n", current.ID); err != nil {
			return err
		}
		if requireDaemon {
			stateDir, err := paths.StateDir()
			if err != nil {
				return err
			}
			socketPath = meshdaemon.SocketPath(stateDir)
		}
	} else {
		if len(command) == 0 {
			command = []string{defaultShell()}
		}
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			return cwdErr
		}
		current, socketPath, err = a.createLocalSession(cmd, command, cwd, requireDaemon)
		if err != nil {
			return err
		}
		initial := uint64(0)
		lastSeq = &initial
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "new session %s\n", current.ID); err != nil {
			return err
		}
	}
	if socketPath == "" {
		socketPath = paths.Socket(current.Dir)
	}
	copy := current
	resolved := resolvedSession{local: &copy}
	opts, err := a.attachmentOptions(cmd, detachKey, raw)
	if err != nil {
		return err
	}
	opts.SocketPath, opts.SessionID, opts.LastSeq = socketPath, current.ID, lastSeq
	opts.ContainingSessions = a.dependencies.Containment(cmd.Context())
	if len(opts.ContainingSessions) > 0 {
		target, err := resolvedTargetIdentity(resolved)
		if err != nil {
			return err
		}
		opts.HostID = target.HostID
	}
	result, err := Attach(opts)
	if err != nil {
		return err
	}
	return reportAttachment(cmd, resolved, result)
}

func reportAttachment(cmd *cobra.Command, resolved resolvedSession, result AttachResult) error {
	id := resolved.local.ID
	switch {
	case result.Exited:
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "\r\nsession %s exited (%d)\r\n", id, result.ExitCode); err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return statusError{code: result.ExitCode}
		}
	case result.Detached:
		_, err := fmt.Fprintf(cmd.ErrOrStderr(), "\r\ndetached from %s, still running\r\n", id)
		return err
	default:
		_, err := fmt.Fprintf(cmd.ErrOrStderr(), "\r\ndisconnected from %s\r\n", id)
		return err
	}
	return nil
}

func (a *application) attachCommand() *cobra.Command {
	var (
		viaDaemon bool
		detachKey string
		raw       bool
	)
	command := &cobra.Command{
		Use:   "attach session",
		Short: "Attach to a session on this host",
		Args:  exactArgs(1, "a session id", "mesh attach 7K3D    (mesh ls lists your sessions)"),
		RunE: func(cmd *cobra.Command, args []string) error {
			current, err := Find(args[0])
			if err != nil {
				return err
			}
			if !current.Alive {
				return fmt.Errorf("session %s is %s", current.ID, current.State())
			}
			opts, err := a.attachmentOptions(cmd, detachKey, raw)
			if err != nil {
				return err
			}
			socketPath := paths.Socket(current.Dir)
			if viaDaemon {
				stateDir, err := paths.StateDir()
				if err != nil {
					return err
				}
				socketPath = meshdaemon.SocketPath(stateDir)
			}
			opts.SocketPath, opts.SessionID = socketPath, current.ID
			opts.ContainingSessions = a.dependencies.Containment(cmd.Context())
			if len(opts.ContainingSessions) > 0 {
				target, err := resolvedTargetIdentity(resolvedSession{local: &current})
				if err != nil {
					return err
				}
				opts.HostID = target.HostID
			}
			result, err := Attach(opts)
			if err != nil {
				return err
			}
			return reportAttachment(cmd, resolvedSession{local: &current}, result)
		},
	}
	command.Flags().BoolVar(&viaDaemon, "daemon", false, "attach through the local daemon")
	command.Flags().StringVar(&detachKey, "detach-key", "", "key that detaches, such as ctrl+] or none")
	command.Flags().BoolVar(&raw, "raw", false, "pass every input byte through")
	return command
}

func (a *application) logsCommand() *cobra.Command {
	var tail int
	var previous bool
	command := &cobra.Command{
		Use:   "logs session",
		Short: "Print recent terminal output without attaching",
		Args:  exactArgs(1, "a session id", "mesh logs 7K3D      (mesh ls lists your sessions)"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tail <= 0 || tail > protocol.MaxLogTail {
				return fmt.Errorf("--tail must be between 1 and %d bytes", protocol.MaxLogTail)
			}
			if previous {
				return a.previousOutput(cmd, args[0], tail)
			}
			hosts, err := LoadHosts()
			if err != nil {
				return err
			}
			resolved, err := a.resolveSession(cmd.Context(), hosts, args[0])
			if err != nil {
				return err
			}
			var output []byte
			if resolved.local != nil {
				if resolved.local.Alive {
					output, err = Logs(*resolved.local, tail)
				} else {
					output, err = worker.ReadLogTail(resolved.local.Dir, tail)
				}
			} else {
				ctx, cancel := context.WithTimeout(cmd.Context(), remoteConnectTimeout)
				output, err = logsRemoteSession(ctx, *resolved.host, a.dependencies.DialHost, resolved.remote.ID, tail)
				cancel()
			}
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(output)
			return err
		},
	}
	command.Flags().IntVar(&tail, "tail", protocol.DefaultLogTail, "maximum trailing bytes to print")
	command.Flags().BoolVar(&previous, "previous", false, "print the saved rendered checkpoint with its timestamp")
	return command
}

func (a *application) killCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "kill session",
		Short: "End a session and wait until it is gone",
		Args:  exactArgs(1, "a session id", "mesh kill 7K3D      (mesh ls lists your sessions)"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSessionControl(cmd, args[0], protocol.TypeKill, "")
		},
	}
}

func (a *application) signalCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "sig session signal",
		Aliases: []string{"signal"},
		Short:   "Send a signal to a session process group",
		Args:    exactArgs(2, "a session id and a signal", "mesh sig 7K3D TERM"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSessionControl(cmd, args[0], protocol.TypeSignal, args[1])
		},
	}
}

func (a *application) removeCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm session...",
		Aliases: []string{"remove"},
		Short:   "Forget finished sessions and delete what they left behind",
		Args:    minimumArgs(1, "at least one session id", "mesh rm 7K3D        (mesh ls lists your sessions)"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := LoadHosts()
			if err != nil {
				return err
			}
			// Keep going after a failure so one bad ID does not strand the rest,
			// and report every outcome rather than the first.
			var failures []error
			for _, id := range args {
				if err := a.removeSession(cmd, hosts, id); err != nil {
					failures = append(failures, err)
					continue
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", strings.ToUpper(id)); err != nil {
					return err
				}
			}
			return errors.Join(failures...)
		},
	}
}

func (a *application) removeSession(cmd *cobra.Command, hosts []HostRecord, id string) error {
	resolved, err := a.resolveSession(cmd.Context(), hosts, id)
	if err != nil {
		return err
	}
	if resolved.local != nil {
		if resolved.local.Meta.State != worker.StateExited && resolved.local.Alive {
			return fmt.Errorf("session %s is still running; kill it before removing it", resolved.local.ID)
		}
		return RemoveLocal(*resolved.local)
	}
	if resolved.remote.State == string(storage.StateRunning) || resolved.remote.State == string(storage.StateDetached) {
		return fmt.Errorf("session %s on %s is still %s; kill it before removing it", resolved.remote.ID, resolved.host.Alias, resolved.remote.State)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Second)
	defer cancel()
	return controlRemoteSession(ctx, *resolved.host, a.dependencies.DialHost, resolved.remote.ID, protocol.TypeRemove, "")
}

func (a *application) runSessionControl(cmd *cobra.Command, id, controlType, signal string) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	resolved, err := a.resolveSession(cmd.Context(), hosts, id)
	if err != nil {
		return err
	}
	if resolved.local != nil {
		// Refuse only on a state the worker actually recorded. Alive is a
		// 500ms socket dial, and a loaded machine can miss it for a perfectly
		// healthy session — which is exactly when kill and sig matter most.
		// Attempt the operation and let the worker's own answer decide.
		if resolved.local.Meta.State == worker.StateExited {
			return fmt.Errorf("session %s is already exited", resolved.local.ID)
		}
		if controlType == protocol.TypeKill {
			err = Kill(*resolved.local)
		} else {
			err = Signal(*resolved.local, signal)
		}
		if err != nil && !resolved.local.Alive {
			// The worker really is unreachable, so the probe was right after
			// all: report the session's state rather than a dial failure.
			return fmt.Errorf("session %s is already interrupted", resolved.local.ID)
		}
	} else {
		if resolved.remote.State != string(storage.StateRunning) && resolved.remote.State != string(storage.StateDetached) {
			return fmt.Errorf("session %s on %s is already %s", resolved.remote.ID, resolved.host.Alias, resolved.remote.State)
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Second)
		err = controlRemoteSession(ctx, *resolved.host, a.dependencies.DialHost, resolved.remote.ID, controlType, signal)
		cancel()
	}
	if err != nil {
		return err
	}
	if controlType == protocol.TypeKill {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "killed %s\n", strings.ToUpper(id))
	} else {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "sent %s to %s\n", signal, strings.ToUpper(id))
	}
	return err
}

func (a *application) daemonCommand() *cobra.Command {
	var (
		port               uint
		sshPort            uint
		path               string
		httpsPort          uint
		certificateRenewer string
		privateNamesConfig string
		edgeConfig         string
		publicEdgeTarget   string
		tailscaleServe     bool
	)
	command := &cobra.Command{
		Use:   "daemon",
		Short: "Run this host's coordinator",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if port > 65535 {
				return fmt.Errorf("tailnet port %d is out of range", port)
			}
			if sshPort > 65535 {
				return fmt.Errorf("SSH port %d is out of range", sshPort)
			}
			if httpsPort > 65535 {
				return fmt.Errorf("HTTPS port %d is out of range", httpsPort)
			}
			stateDir, err := paths.StateDir()
			if err != nil {
				return err
			}
			return meshdaemon.Run(cmd.Context(), meshdaemon.Config{
				SSHSessionHandler: a.dependencies.SSHSessionHandler,
				StateDir:          stateDir, TailnetPort: uint16(port), SSHPort: uint16(sshPort), WebSocketPath: path, HTTPSPort: uint16(httpsPort),
				CertificateRenewerID: certificateRenewer, PrivateNamesConfig: privateNamesConfig,
				EdgeConfig: edgeConfig, PublicEdgeTarget: publicEdgeTarget,
				TailscaleServe: tailscaleServe,
				ReportError:    func(err error) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), "mesh daemon: %v\n", err) },
			})
		},
	}
	command.Flags().UintVar(&port, "tailnet-port", 0, "Tailnet WebSocket port; zero disables remote listening")
	command.Flags().UintVar(&sshPort, "ssh-port", 0, "Tailnet SSH port; zero disables SSH")
	command.Flags().StringVar(&path, "websocket-path", "/mesh", "Tailnet WebSocket path")
	command.Flags().UintVar(&httpsPort, "https-port", 0, "loopback HTTPS service port; zero disables HTTPS")
	command.Flags().StringVar(&certificateRenewer, "certificate-renewer-id", "", "pinned Mesh identity allowed to install certificates")
	command.Flags().StringVar(&privateNamesConfig, "private-names-config", "", "Pi private-name reconciliation config file")
	command.Flags().StringVar(&edgeConfig, "edge", "", "public-edge runtime and origin allowlist config file")
	command.Flags().StringVar(&publicEdgeTarget, "public-edge-target", "", "pinned public-edge target config file")
	command.Flags().BoolVar(&tailscaleServe, "tailscale-serve", false, "persist raw Tailscale TCP/443 forwarding to the HTTPS port")
	return command
}

func (a *application) workerCommand() *cobra.Command {
	var recoveredFrom string
	var (
		id   string
		dir  string
		cols int
		rows int
	)
	command := &cobra.Command{
		Use:    "session-worker [-- command...]",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, command []string) error {
			if id == "" || dir == "" {
				return errors.New("session-worker requires --id and --dir")
			}
			cwd, _ := os.Getwd()
			code, err := worker.Run(worker.Config{
				ID: id, Dir: dir, Command: command, Cwd: cwd, Env: os.Environ(),
				Cols: cols, Rows: rows, AwaitInitialAttach: true, RecoveredFrom: recoveredFrom,
			})
			if err != nil {
				return err
			}
			if code != 0 {
				return statusError{code: code}
			}
			return nil
		},
	}
	command.Flags().StringVar(&recoveredFrom, "recovered-from", "", "previous recovery attempt")
	command.Flags().StringVar(&id, "id", "", "session ID")
	command.Flags().StringVar(&dir, "dir", "", "session state directory")
	command.Flags().IntVar(&cols, "cols", 80, "initial terminal width")
	command.Flags().IntVar(&rows, "rows", 24, "initial terminal height")
	return command
}

func terminalSize(output *os.File) (int, int) {
	if width, height, err := term.GetSize(output.Fd()); err == nil && width > 0 && height > 0 {
		return width, height
	}
	return 80, 24
}

func defaultShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "/bin/sh"
}

func ageAt(now, created time.Time) string {
	duration := now.Sub(created).Round(time.Second)
	if duration < 0 {
		duration = 0
	}
	switch {
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	case duration < 24*time.Hour:
		return fmt.Sprintf("%dh", int(duration.Hours()))
	default:
		return fmt.Sprintf("%dd", int(duration.Hours()/24))
	}
}

// localHostAlias labels this machine's own sessions in a merged listing. It is
// not an address-book entry: a host never adopts itself.
const localHostAlias = "this host"

// localSessionRows renders this machine's sessions in the same shape the remote
// fan-out returns, so one listing can carry both.
func localSessionRows() ([]protocol.SessionInfo, error) {
	sessions, err := List()
	if err != nil {
		return nil, err
	}
	config, err := localRecoveryConfig()
	if err != nil {
		return nil, err
	}
	rows := make([]protocol.SessionInfo, 0, len(sessions))
	for _, current := range sessions {
		row := protocol.SessionInfo{
			ID:        current.ID,
			Command:   append([]string(nil), current.Command...),
			Cwd:       current.Cwd,
			State:     current.State(),
			CreatedAt: current.CreatedAt,
		}
		if current.ExitCode != nil {
			code := *current.ExitCode
			row.ExitCode = &code
		}
		addLocalRecoveryInfo(&row, current, config.HostID)
		rows = append(rows, row)
	}
	return rows, nil
}

// Command groups for the help listing.
const (
	groupSessions = "sessions"
	groupHosts    = "hosts"
	groupServing  = "serving"
	groupSetup    = "setup"
)

func inGroup(id string, commands ...*cobra.Command) []*cobra.Command {
	for _, command := range commands {
		command.GroupID = id
	}
	return commands
}

// pickerSessionAction targets the exact host already open in the picker. It is
// deliberately separate from standalone kill/rm, which resolve global or local
// IDs and print command output that would corrupt the picker's alternate screen.
func (a *application) pickerSessionAction(ctx context.Context, hosts []HostRecord, request PickerSessionActionRequest) error {
	if ctx == nil {
		return errors.New("picker session action with nil context")
	}
	if request.HostAlias == localHostAlias {
		return a.localPickerSessionAction(ctx, request)
	}
	host, err := hostWithAlias(hosts, request.HostAlias)
	if err != nil {
		return err
	}
	id, err := session.ParseID(request.SessionID)
	if err != nil {
		return fmt.Errorf("picker session action: %w", err)
	}
	var controlType string
	switch request.Action {
	case PickerKillSession:
		controlType = protocol.TypeKill
	case PickerRemoveSession:
		controlType = protocol.TypeRemove
	default:
		return fmt.Errorf("unknown picker session action %d", request.Action)
	}
	actionContext, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return controlRemoteSession(actionContext, host, a.dependencies.DialControl, id, controlType, "")
}
