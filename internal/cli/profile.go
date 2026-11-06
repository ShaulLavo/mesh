package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
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
	group := &cobra.Command{Use: "profile", Short: "Read and edit this machine's owner-written notes", RunE: func(_ *cobra.Command, _ []string) error {
		return agentapi.Usage("profile.show", "Choose mesh profile show, set or edit.")
	}}
	group.Flags().Bool("json", false, "write one agent result")
	group.AddCommand(a.profileShowCommand(), a.profileSetCommand(), a.profileEditCommand())
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
	var asJSON bool
	command := &cobra.Command{Use: "show", Short: "Read the local host profile without contacting the daemon", Args: profileArgs("profile.show"), RunE: func(cmd *cobra.Command, _ []string) error {
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
	var file string
	var expected uint64
	var asJSON bool
	command := &cobra.Command{Use: "set", Short: "Replace the local host profile at an expected revision", Args: profileArgs("profile.set"), RunE: func(cmd *cobra.Command, _ []string) error {
		if !asJSON || !cmd.Flags().Changed("file") || file == "" || !cmd.Flags().Changed("expected-revision") {
			return agentapi.Usage("profile.set", "Use --file PATH|- --expected-revision N --json.")
		}
		return a.setProfile(cmd, file, expected)
	}}
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
		return profileFailure(err, "profile.set")
	}
	p, changed, err := store.Set(cmd.Context(), expected, next)
	if err != nil {
		return profileFailure(err, "profile.set")
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

func profileFailure(err error, operation string) error {
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
	var asJSON bool
	command := &cobra.Command{Use: "edit", Short: "Edit the local profile with VISUAL or EDITOR, for humans only", Args: profileArgs("profile.edit"), RunE: func(cmd *cobra.Command, _ []string) error {
		if asJSON {
			return agentapi.Usage("profile.edit", "The editor is human-only. Use mesh profile set --file PATH --expected-revision N --json.")
		}
		return editProfile(cmd)
	}}
	command.Flags().BoolVar(&asJSON, "json", false, "unsupported; use profile set for JSON input")
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
	return finishProfileEdit(cmd, store, temporary, *snapshot.Revision)
}

func finishProfileEdit(cmd *cobra.Command, store *hostprofile.HostStore, temporary *hostprofile.EditFile, revision uint64) error {
	data, err := temporary.Read()
	if err == nil {
		var next hostprofile.Editable
		next, err = hostprofile.ParseEditable(data, hostprofile.HostLimits)
		if err == nil {
			_, _, err = store.Set(cmd.Context(), revision, next)
		}
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
