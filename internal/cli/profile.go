package cli

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"math"
	"time"

	"encoding/json"
	"errors"
	"fmt"
	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/update"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/spf13/cobra"
)

func (a *application) profileCommand() *cobra.Command {
	group := &cobra.Command{Use: "profile", Short: "Read and edit owner-written host and fleet notes", RunE: func(_ *cobra.Command, _ []string) error {
		return agentapi.Usage("profile.show", "Choose mesh profile show, set, edit or status.")
	}}
	group.Flags().Bool("json", false, "write one agent result")
	group.AddCommand(a.profileShowCommand(), a.profileSetCommand(), a.profileEditCommand(), a.profileStatusCommand())
	return group
}

func profileArgs(operation string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return agentapi.Usage(operation, "This command takes no positional arguments.")
		}
		return nil
	}
}

func (a *application) profileShowCommand() *cobra.Command {
	var fleet bool
	var asJSON bool
	command := &cobra.Command{Use: "show", Short: "Read the local host or fleet profile without contacting peers", Args: profileArgs("profile.show"), RunE: func(cmd *cobra.Command, _ []string) error {
		if fleet {
			return a.showFleetProfile(cmd, asJSON)
		}
		store, _, err := localProfileStore()
		if err != nil {
			return err
		}
		snapshot, err := store.Read()
		if err != nil {
			return fmt.Errorf("read local profile: %w", err)
		}
		section, err := profileSection(snapshot)
		if err != nil {
			return err
		}
		if asJSON {
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: section})
		}
		if a.privacy != nil {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Profile text hidden by privacy mode")
			if err != nil {
				return fmt.Errorf("print profile privacy notice: %w", err)
			}
			return nil
		}
		return printProfile(cmd.OutOrStdout(), section)
	}}
	command.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	command.Flags().BoolVar(&fleet, "fleet", false, "read the local signed fleet replica")
	return command
}

func localProfileStore() (*hostprofile.HostStore, string, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, "", fmt.Errorf("locate profile identity: %w", err)
	}
	host, err := identity.Load(state)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", agentapi.Fail(agentapi.HostUninitialized, "", "", "This machine has no Mesh identity yet. Set up Mesh here first with mesh install.")
	}
	if err != nil {
		return nil, "", fmt.Errorf("read local profile identity: %w", err)
	}
	config, err := ConfigPath()
	if err != nil {
		return nil, "", err
	}
	return hostprofile.OpenHostStore(filepath.Dir(config), host.ID), state, nil
}

func profileSection(snapshot hostprofile.Snapshot[hostprofile.HostProfile]) (agentapi.ProfileSection, error) {
	var state agentapi.ProfileState
	switch snapshot.State {
	case hostprofile.StateAbsent:
		state = "absent"
	case hostprofile.StateValid:
		state = "valid"
	case hostprofile.StateInvalid:
		state = "invalid"
	case hostprofile.StateUnavailable:
		state = "unavailable"
	case hostprofile.StateUntrusted:
		state = "untrusted"
	default:
		return agentapi.ProfileSection{}, errors.New("unknown stored profile state")
	}
	section := agentapi.ProfileSection{State: state, Completeness: agentapi.CompletenessFull, Revision: snapshot.Revision, Problem: agentapi.CleanMessage(snapshot.Problem), Freshness: agentapi.FreshnessLive}
	if snapshot.Value == nil {
		return section, nil
	}
	p := snapshot.Value
	section.Digest = p.Digest()
	section.UpdatedAt = &p.UpdatedAt
	section.Profile = &agentapi.Profile{Description: p.Description, Labels: p.Labels, Warnings: p.Warnings, Instructions: &p.Instructions, InstructionsBytes: len(p.Instructions)}
	return section, nil
}

func printProfile(output io.Writer, section agentapi.ProfileSection) error {
	if section.Profile == nil {
		_, err := fmt.Fprintf(output, "Profile: %s\n%s\n", section.State, section.Problem)
		return wrapProfileOutput(err)
	}
	p := section.Profile
	if _, err := fmt.Fprintf(output, "%s\nRevision: %d\n", p.Description, *section.Revision); err != nil {
		return fmt.Errorf("print profile: %w", err)
	}
	keys := make([]string, 0, len(p.Labels))
	for key := range p.Labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(output, "%s: %s\n", key, p.Labels[key]); err != nil {
			return fmt.Errorf("print profile label: %w", err)
		}
	}
	for _, warning := range p.Warnings {
		if _, err := fmt.Fprintf(output, "Warning: %s\n", warning); err != nil {
			return fmt.Errorf("print profile warning: %w", err)
		}
	}
	_, err := fmt.Fprintln(output, *p.Instructions)
	return wrapProfileOutput(err)
}

func wrapProfileOutput(err error) error {
	if err != nil {
		return fmt.Errorf("print profile: %w", err)
	}
	return nil
}

func (a *application) profileSetCommand() *cobra.Command {
	var fleet bool
	var file string
	var expected uint64
	var asJSON bool
	command := &cobra.Command{Use: "set", Short: "Replace host or fleet notes at an expected revision", Args: profileArgs("profile.set"), RunE: func(cmd *cobra.Command, _ []string) error {
		if !asJSON || !cmd.Flags().Changed("file") || file == "" || !cmd.Flags().Changed("expected-revision") {
			return agentapi.Usage("profile.set", "Use --file PATH|- --expected-revision N --json.")
		}
		if fleet {
			return a.setFleetProfile(cmd, file, expected)
		}
		return a.setProfile(cmd, file, expected)
	}}
	command.Flags().BoolVar(&fleet, "fleet", false, "replace signed fleet notes and queue saved members")
	command.Flags().StringVar(&file, "file", "", "editable JSON path, or - to explicitly read stdin")
	command.Flags().Uint64Var(&expected, "expected-revision", 0, "revision read before this edit; 0 for an absent profile")
	command.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return command
}

func (a *application) setProfile(cmd *cobra.Command, file string, expected uint64) error {
	store, _, err := localProfileStore()
	if err != nil {
		return err
	}
	data, err := readProfileInput(cmd, file)
	if err != nil {
		return err
	}
	next, err := hostprofile.ParseEditable(data, hostprofile.HostLimits)
	if err != nil {
		return profileFailure(err)
	}
	p, changed, err := store.Set(cmd.Context(), expected, next)
	if err != nil {
		return profileFailure(err)
	}
	result := agentapi.ProfileWrite{Revision: p.Revision, Digest: p.Digest(), Changed: changed, Committed: true, Publication: "complete", Members: []agentapi.Delivery{}}
	return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: result})
}

func readProfileInput(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return boundedProfileInput(cmd.InOrStdin())
	}
	// #nosec G304 -- The local caller explicitly supplies --file; this is not a peer path.
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open profile input: %w", err)
	}
	defer func() { _ = file.Close() }()
	return boundedProfileInput(file)
}

func boundedProfileInput(input io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, int64(hostprofile.HostLimits.Encoded)+1))
	if err != nil {
		return nil, fmt.Errorf("read profile input: %w", err)
	}
	if len(data) > hostprofile.HostLimits.Encoded {
		return nil, agentapi.Fail(agentapi.ProfileInvalid, "", "", "Profile document exceeds 32768 encoded bytes.")
	}
	return data, nil
}

func profileFailure(err error) error {
	const operation = "profile.set"
	var validation *hostprofile.ValidationError
	var conflict *hostprofile.ConflictError
	switch {
	case errors.As(err, &validation):
		return agentapi.Fail(agentapi.ProfileInvalid, operation, "profile.json", err.Error())
	case errors.As(err, &conflict):
		return agentapi.Fail(agentapi.ProfileConflict, operation, "profile.json", err.Error())
	default:
		return fmt.Errorf("local profile: %w", err)
	}
}

func (a *application) profileEditCommand() *cobra.Command {
	var fleet bool
	var asJSON bool
	command := &cobra.Command{Use: "edit", Short: "Edit the local profile with VISUAL or EDITOR, for humans only", Args: profileArgs("profile.edit"), RunE: func(cmd *cobra.Command, _ []string) error {
		if asJSON {
			return agentapi.Usage("profile.edit", "The editor is human-only. Use mesh profile set --file PATH --expected-revision N --json.")
		}
		if fleet {
			return editFleetProfile(cmd)
		}
		return editProfile(cmd)
	}}
	command.Flags().BoolVar(&asJSON, "json", false, "unsupported; use profile set for JSON input")
	command.Flags().BoolVar(&fleet, "fleet", false, "edit signed fleet notes")
	return command
}

func editProfile(cmd *cobra.Command) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if strings.TrimSpace(editor) == "" {
		return errors.New("Set VISUAL or EDITOR to edit the profile.")
	}
	store, state, err := localProfileStore()
	if err != nil {
		return err
	}
	snapshot, err := store.Read()
	if err != nil {
		return fmt.Errorf("read editable profile: %w", err)
	}
	if snapshot.Revision == nil {
		return errors.New("Back up and repair profile.json before editing its damaged revision.")
	}
	next := hostprofile.Editable{SchemaVersion: hostprofile.SchemaVersion, Labels: map[string]string{}, Warnings: []string{}}
	if snapshot.Value != nil {
		next = snapshot.Value.Editable
	}
	return editProfileDocument(cmd, state, next, hostprofile.HostLimits, func(next hostprofile.Editable) error {
		_, _, err := store.Set(cmd.Context(), *snapshot.Revision, next)
		return wrapProfileError(err)
	})
}

func editProfileDocument(cmd *cobra.Command, state string, next hostprofile.Editable, limits hostprofile.Limits, save func(hostprofile.Editable) error) error {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode editable profile: %w", err)
	}
	temporary, err := hostprofile.NewEditFile(state, data)
	if err != nil {
		return fmt.Errorf("prepare editable profile: %w", err)
	}
	defer temporary.Close()
	// #nosec G204 G702 -- Human-only execution of the local operator's VISUAL/EDITOR; no profile text is evaluated.
	process := exec.CommandContext(cmd.Context(), "sh", "-c", editor+` "$1"`, "mesh-profile-editor", temporary.Path())
	process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("profile editor: %w; editable copy kept at %s", err, temporary.Path())
	}
	return finishProfileEdit(cmd, temporary, limits, save)
}

func finishProfileEdit(cmd *cobra.Command, temporary *hostprofile.EditFile, limits hostprofile.Limits, save func(hostprofile.Editable) error) error {
	data, err := readEditedProfile(temporary, limits)
	if err == nil {
		err = saveEditedProfile(data, limits, save)
	}
	if err != nil {
		return fmt.Errorf("profile edit: %w; editable copy kept at %s", err, temporary.Path())
	}
	if err := temporary.Remove(); err != nil {
		return fmt.Errorf("clean editable profile: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Profile saved.")
	return wrapProfileOutput(err)
}
func readEditedProfile(temporary *hostprofile.EditFile, limits hostprofile.Limits) ([]byte, error) {
	var data []byte
	var err error
	if limits == hostprofile.HostLimits {
		data, err = temporary.Read()
	} else {
		data, err = temporary.ReadFleet()
	}
	return data, wrapProfileError(err)
}

func saveEditedProfile(data []byte, limits hostprofile.Limits, save func(hostprofile.Editable) error) error {
	next, err := hostprofile.ParseEditable(data, limits)
	if err != nil {
		return fmt.Errorf("parse edited profile: %w", err)
	}
	return save(next)
}

const profileStatusVerb = "status"

type localFleetProfile struct {
	state string
	id    string
	key   ed25519.PrivateKey
	store *hostprofile.FleetStore
	scope hostprofile.FleetScope
	saved bool
	name  string
}

func openLocalFleetProfile() (localFleetProfile, error) {
	local := localFleetProfile{}
	state, err := paths.StateDirPath()
	if err != nil {
		return local, fmt.Errorf("locate fleet state: %w", err)
	}
	host, key, err := identity.LoadPrivate(state)
	if errors.Is(err, os.ErrNotExist) {
		return local, agentapi.Fail(agentapi.HostUninitialized, "", "", "This machine has no Mesh identity yet. Set up Mesh here first with mesh install.")
	}
	if err != nil {
		return local, fmt.Errorf("read fleet signing identity: %w", err)
	}
	config, err := ConfigPath()
	if err != nil {
		return local, err
	}
	authority := &update.Authority{StateDir: state, ID: host.ID}
	local = localFleetProfile{state: state, id: host.ID, key: key, store: hostprofile.OpenFleetStore(state), scope: hostprofile.FleetScope{Trusted: authority.IsAdministrator, Members: []string{}}}
	fleet, err := update.ReadFleet(filepath.Join(filepath.Dir(config), "fleet.json"))
	if errors.Is(err, os.ErrNotExist) {
		return local, nil
	}
	if err != nil {
		return local, fmt.Errorf("read saved fleet: %w", err)
	}
	local.saved, local.name = true, fleet.Name
	for _, member := range fleet.Members {
		if member.ID != host.ID {
			local.scope.Members = append(local.scope.Members, member.ID)
		}
	}
	return local, nil
}
func (a *application) showFleetProfile(cmd *cobra.Command, asJSON bool) error {
	local, err := openLocalFleetProfile()
	if err != nil {
		return err
	}
	snapshot, err := local.store.ReadReplica(local.scope.Trusted)
	if err != nil {
		return fmt.Errorf("read fleet profile: %w", err)
	}
	section := fleetProfileSection(snapshot)
	if asJSON {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: section})
	}
	if a.privacy != nil {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Profile text hidden by privacy mode")
		return wrapProfileOutput(err)
	}
	return printProfile(cmd.OutOrStdout(), section.ProfileSection)
}
func fleetProfileSection(snapshot hostprofile.Snapshot[hostprofile.FleetDocument]) agentapi.FleetSection {
	wire := hostprofile.FleetSection(snapshot, true)
	section := agentContextSection(wire, agentapi.FreshnessLive, time.Now().UTC())
	return agentapi.FleetSection{ProfileSection: section, Fleet: wire.Fleet, Author: wire.Author}
}
func (a *application) setFleetProfile(cmd *cobra.Command, file string, expected uint64) error {
	local, err := openLocalFleetProfile()
	if err != nil {
		return err
	}
	next, err := readFleetInput(cmd, file)
	if err != nil {
		return profileFailure(err)
	}
	result, err := local.write(cmd.Context(), expected, next)
	if err == nil {
		pokeFleetProfile(cmd.Context(), local.state)
	}
	data := fleetWriteResult(result, local.saved)
	if err == nil {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
	}
	if result.Publication != hostprofile.ReplicaDurable {
		return profileFailure(err)
	}
	problem := agentError(err, "profile.set", "fleet/profile.json", agentapi.StateUnavailable)
	return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{problem}})
}
func readFleetInput(cmd *cobra.Command, path string) (hostprofile.Editable, error) {
	input := cmd.InOrStdin()
	if path != "-" {
		// #nosec G304 -- --file is explicitly supplied by the local caller.
		file, err := os.Open(path)
		if err != nil {
			return hostprofile.Editable{}, fmt.Errorf("open fleet input: %w", err)
		}
		defer func() { _ = file.Close() }()
		input = file
	}
	data, err := io.ReadAll(io.LimitReader(input, int64(hostprofile.FleetLimits.Encoded)+1))
	if err != nil {
		return hostprofile.Editable{}, fmt.Errorf("read fleet input: %w", err)
	}
	next, err := hostprofile.ParseEditable(data, hostprofile.FleetLimits)
	return next, wrapProfileError(err)
}
func (local localFleetProfile) write(ctx context.Context, expected uint64, next hostprofile.Editable) (hostprofile.FleetWrite, error) {
	if expected == math.MaxUint64 {
		return hostprofile.FleetWrite{}, errors.New("fleet revision exhausted")
	}
	doc, err := hostprofile.SignFleet(next, local.name, expected+1, time.Now().UTC(), local.id, local.key)
	if err != nil {
		return hostprofile.FleetWrite{}, fmt.Errorf("sign fleet profile: %w", err)
	}
	result, err := local.store.WriteLocal(ctx, expected, doc, local.scope)
	return result, wrapProfileError(err)
}
func fleetWriteResult(result hostprofile.FleetWrite, saved bool) agentapi.ProfileWrite {
	return agentapi.ProfileWrite{Revision: result.Revision, Digest: result.Digest, Changed: result.Changed, Committed: result.Committed(), Publication: string(result.Publication), Queued: result.Complete(), FleetSaved: saved, Members: fleetDeliveries(result.Outbox.Members)}
}
func fleetDeliveries(members []hostprofile.Delivery) []agentapi.Delivery {
	result := make([]agentapi.Delivery, 0, len(members))
	for _, member := range members {
		delivery := agentapi.Delivery{HostID: member.HostID, State: string(member.State), QueuedAt: member.QueuedAt, SettledAt: member.SettledAt, MemberRevision: member.MemberRevision, MemberDigest: member.MemberDigest}
		if member.LastError != nil {
			delivery.LastError = &agentapi.DeliveryError{Code: fleetDeliveryCode(member.LastError.Code), Message: agentapi.CleanMessage(member.LastError.Message), Retryable: member.LastError.Retryable}
		}
		result = append(result, delivery)
	}
	return result
}
func pokeFleetProfile(ctx context.Context, state string) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	// The poke is optional. A stopped daemon is never started and disk remains authoritative.
	_, _ = daemonControlRequest(ctx, meshdaemon.SocketPath(state), protocol.Control{Type: protocol.TypeFleetProfileSync})
}
func (a *application) profileStatusCommand() *cobra.Command {
	var fleet, asJSON, retry bool
	command := &cobra.Command{Use: profileStatusVerb, Short: "Read local fleet delivery state without contacting members", Args: profileArgs("profile.status"), RunE: func(cmd *cobra.Command, _ []string) error {
		if !fleet || !asJSON {
			return agentapi.Usage("profile.status", "Use mesh profile status --fleet --json [--retry].")
		}
		return a.fleetProfileStatus(cmd, retry)
	}}
	command.Flags().BoolVar(&fleet, "fleet", false, "read fleet delivery state")
	command.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	command.Flags().BoolVar(&retry, "retry", false, "reset pending and stale delivery age and retry")
	return command
}
func (a *application) fleetProfileStatus(cmd *cobra.Command, retry bool) error {
	local, err := openLocalFleetProfile()
	if err != nil {
		return err
	}
	if retry {
		if _, err := local.store.Requeue(cmd.Context(), local.scope); err != nil {
			return fmt.Errorf("retry fleet delivery: %w", err)
		}
		pokeFleetProfile(cmd.Context(), local.state)
	}
	snapshot, out, err := local.store.Read(local.scope.Trusted)
	data := agentapi.FleetStatus{Replica: fleetProfileSection(snapshot), FleetSaved: local.saved, Fleet: local.name, Members: fleetDeliveries(out.Members)}
	if err != nil {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{agentError(err, "profile.status", "fleet/outbox.json", agentapi.StateUnavailable)}})
	}
	return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
}
func editFleetProfile(cmd *cobra.Command) error {
	if strings.TrimSpace(cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"))) == "" {
		return errors.New("Set VISUAL or EDITOR to edit the profile.")
	}
	local, err := openLocalFleetProfile()
	if err != nil {
		return err
	}
	snapshot, err := local.store.ReadReplica(local.scope.Trusted)
	if err != nil {
		return fmt.Errorf("read fleet profile: %w", err)
	}
	if snapshot.Revision == nil {
		return errors.New("Back up and repair fleet/profile.json before editing its damaged revision.")
	}
	next := hostprofile.Editable{SchemaVersion: hostprofile.SchemaVersion, Labels: map[string]string{}, Warnings: []string{}}
	if snapshot.Value != nil {
		next = snapshot.Value.Editable
	}
	return editProfileDocument(cmd, local.state, next, hostprofile.FleetLimits, func(next hostprofile.Editable) error {
		_, err := local.write(cmd.Context(), *snapshot.Revision, next)
		if err == nil {
			pokeFleetProfile(cmd.Context(), local.state)
		}
		return err
	})
}

func wrapProfileError(err error) error {
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	return nil
}

func fleetDeliveryCode(code hostprofile.DeliveryCode) agentapi.Code {
	switch code {
	case hostprofile.DeliveryUnreachable:
		return agentapi.HostUnreachable
	case hostprofile.DeliveryAuthenticationFailed:
		return agentapi.HostAuthenticationFailed
	case hostprofile.DeliveryRejected:
		return agentapi.FleetDeliveryRejected
	case hostprofile.DeliverySignerUntrusted:
		return agentapi.FleetSignerUntrusted
	case hostprofile.DeliveryRevisionConflict:
		return agentapi.FleetRevisionConflict
	case hostprofile.DeliveryStale:
		return agentapi.FleetStale
	}
	return agentapi.InternalError
}
