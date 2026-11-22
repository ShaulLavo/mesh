package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpgateway"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/spf13/cobra"
)

const (
	mcpProfileTimeout     = 2 * time.Second
	mcpProfileCommandName = "profile"
)

type mcpProfileFlags struct {
	asJSON           bool
	expectedRevision uint64
	identityName     string
}

func (f *mcpProfileFlags) bind(cmd *cobra.Command, identity bool) {
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "write one agent result")
	cmd.Flags().Uint64Var(&f.expectedRevision, "expected-revision", 0, "fail unless mcp-profiles.json is still at this revision")
	if identity {
		cmd.Flags().StringVar(&f.identityName, "identity", "", "read the host catalog as this application identity")
	}
}

func (f *mcpProfileFlags) expected(cmd *cobra.Command) *uint64 {
	if !cmd.Flags().Changed("expected-revision") {
		return nil
	}
	value := f.expectedRevision
	return &value
}

func (a *application) mcpProfileCommand() *cobra.Command {
	profile := &cobra.Command{Use: mcpProfileCommandName, Short: "Manage aggregate gateway profiles in mcp-profiles.json", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	}}
	profile.SetOut(a.dependencies.Stdout)
	profile.SetErr(a.dependencies.Stderr)
	profile.AddCommand(a.mcpProfileShowCommand(), a.mcpProfileAddCommand(), a.mcpProfileRemoveCommand(), a.mcpProfileAliasCommand(), a.mcpProfileRepairCommand())
	return profile
}

func profileStore() (*mcpgateway.Store, error) {
	dir, err := meshConfigDir()
	if err != nil {
		return nil, err
	}
	return mcpgateway.NewStore(dir), nil
}

func profileUsage(operation string, err error) error {
	switch {
	case errors.Is(err, mcpgateway.ErrConflict):
		return agentapi.Fail(agentapi.FleetRevisionConflict, operation, mcpgateway.ProfilesFileName, err.Error()+". Read it again with mesh mcp profile show --json.")
	case errors.Is(err, mcpgateway.ErrNotFound), errors.Is(err, mcpgateway.ErrInvalid), errors.Is(err, mcpgateway.ErrLimit):
		return agentapi.Usage(operation, err.Error())
	default:
		return err
	}
}

func (a *application) mcpProfileShowCommand() *cobra.Command {
	var flags mcpProfileFlags
	var cached bool
	cmd := &cobra.Command{Use: "show [NAME]", Short: "Show gateway profiles and the state of each selected service", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store, err := profileStore()
		if err != nil {
			return err
		}
		file, err := store.Load()
		if err != nil {
			return profileUsage("mcp.profile.show", err)
		}
		names := slices.Sorted(maps.Keys(file.Profiles))
		if len(args) == 1 {
			if _, ok := file.Profiles[args[0]]; !ok {
				return agentapi.Usage("mcp.profile.show", fmt.Sprintf("No MCP profile named %q.", args[0]))
			}
			names = []string{args[0]}
		}
		data := profilesData(store, file, names, false)
		if !cached {
			if err := a.annotateProfileStates(cmd.Context(), &data, flags.identityName); err != nil {
				return err
			}
		}
		return a.writeProfiles(cmd, flags.asJSON, data)
	}}
	flags.bind(cmd, true)
	cmd.Flags().BoolVar(&cached, "cached", false, "show the file only, without asking hosts for service states")
	return cmd
}

func profilesData(store *mcpgateway.Store, file mcpgateway.File, names []string, changed bool) agentapi.MCPProfilesData {
	data := agentapi.MCPProfilesData{Revision: file.Revision, Path: filepath.Join(store.Dir(), mcpgateway.ProfilesFileName),
		Profiles: []agentapi.MCPProfile{}, OverflowNames: len(file.Overflow.Names), Changed: changed}
	for _, name := range names {
		p := file.Profiles[name]
		out := agentapi.MCPProfile{Name: name, Native: p.Native, Services: []agentapi.MCPProfileEntry{}}
		for _, s := range p.Services {
			out.Services = append(out.Services, agentapi.MCPProfileEntry{Alias: s.Alias, HostID: s.HostID, ServiceID: s.ServiceID,
				ServiceName: s.ServiceName, Instance: s.Instance, Include: nonNil(s.Include), Exclude: nonNil(s.Exclude)})
		}
		data.Profiles = append(data.Profiles, out)
	}
	return data
}

// profileCatalogCaller is the credential profile commands read catalogs
// with: the named identity's, or whole-account administration.
func profileCatalogCaller(identityName string) (protocol.CallerCredential, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return "", fmt.Errorf("locate Mesh state directory: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return "", err
	}
	caller, _, err := resolveMCPListCaller(identityName, local.ID)
	return caller, err
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// annotateProfileStates reads each selected host's passive catalog once,
// concurrently, within the fleet budget. It never starts a service.
func (a *application) annotateProfileStates(ctx context.Context, data *agentapi.MCPProfilesData, identityName string) error {
	caller, err := profileCatalogCaller(identityName)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, mcpProfileTimeout)
	defer cancel()
	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()
	hostIDs := map[string]bool{}
	for _, p := range data.Profiles {
		for _, s := range p.Services {
			hostIDs[s.HostID] = true
		}
	}
	results := make(map[string]profileHostResult, len(hostIDs))
	done := make(chan profileHostResult, len(hostIDs))
	for hostID := range hostIDs {
		go func() { done <- readProfileHost(ctx, pool, hostID, caller) }()
	}
	for range hostIDs {
		r := <-done
		results[r.hostID] = r
	}
	for i := range data.Profiles {
		for j := range data.Profiles[i].Services {
			applyProfileState(&data.Profiles[i].Services[j], results[data.Profiles[i].Services[j].HostID])
		}
	}
	return nil
}

type profileHostResult struct {
	hostID  string
	machine string
	views   []mcphost.CatalogService
	state   string
	code    agentapi.Code
	message string
}

func readProfileHost(ctx context.Context, pool *mcpHostPool, hostID string, caller protocol.CallerCredential) profileHostResult {
	r := profileHostResult{hostID: hostID}
	host, err := gatewayHosts{}.lookup(hostID)
	if err == nil {
		r.machine = host.MachineName
		r.views, err = fetchMCPCatalogViews(ctx, pool, host, caller)
	}
	if err == nil {
		return r
	}
	code, _ := classifyGatewayTransportError(err)
	r.code, r.message = code, agentapi.CleanMessage(err.Error())
	r.state = agentapi.GatewayOffline
	if state, ok := profileHostStates[code]; ok {
		r.state = state
	}
	return r
}

// profileHostStates names the host failures that are not plain unreachability.
var profileHostStates = map[agentapi.Code]string{
	agentapi.AccessDenied:             agentapi.GatewayUnauthorized,
	agentapi.HostAuthenticationFailed: agentapi.GatewayUnauthorized,
	agentapi.HostIdentityMismatch:     agentapi.GatewayIdentityChanged,
	agentapi.ProtocolUnsupported:      agentapi.GatewayUnsupported,
}

func applyProfileState(entry *agentapi.MCPProfileEntry, r profileHostResult) {
	entry.MachineName = r.machine
	if r.state != "" {
		entry.State, entry.Code, entry.Message = r.state, r.code, r.message
		return
	}
	for _, v := range r.views {
		if v.ID == entry.ServiceID {
			entry.State, entry.Revision = classifyMCPServiceState(v), v.Revision
			return
		}
	}
	entry.State, entry.Code = agentapi.GatewayNotFound, agentapi.ServiceNotFound
	entry.Message = "the selected service ID is no longer published; run mesh mcp profile repair " + entry.Alias
}

func (a *application) writeProfiles(cmd *cobra.Command, asJSON bool, data agentapi.MCPProfilesData) error {
	if asJSON {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
	}
	_, err := io.WriteString(cmd.OutOrStdout(), profilesText(data))
	return executionBoundaryError("print profiles", err)
}

func profilesText(data agentapi.MCPProfilesData) string {
	if len(data.Profiles) == 0 {
		return fmt.Sprintf("No MCP profiles in %s. Add one with mesh mcp profile add NAME.\n", data.Path)
	}
	var b strings.Builder
	for _, p := range data.Profiles {
		native := "all"
		if p.Native != nil {
			native = strings.Join(append([]string{"mesh_skill"}, p.Native...), ",")
		}
		fmt.Fprintf(&b, "%s\tnative=%s\n", p.Name, native)
		for _, s := range p.Services {
			state := s.State
			if state == "" {
				state = "-"
			}
			fmt.Fprintf(&b, "  %s\t%s/%s\t%s\n", s.Alias, mcpHostLabel(s.HostID, s.MachineName, false), s.ServiceName, state)
		}
	}
	return b.String()
}

type mcpProfileAddOptions struct {
	mcpProfileFlags
	alias    string
	native   string
	include  []string
	exclude  []string
	instance string
}

func (a *application) mcpProfileAddCommand() *cobra.Command {
	var o mcpProfileAddOptions
	cmd := &cobra.Command{Use: "add NAME [HOST/SERVICE]", Short: "Create a profile or select one hosted service in it", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		return a.runMCPProfileAdd(cmd, o, args)
	}}
	o.bind(cmd, true)
	cmd.Flags().StringVar(&o.alias, "alias", "", "display alias for the service (default: its name)")
	cmd.Flags().StringVar(&o.native, "native", "", "native groups: all, none, or a comma list of "+strings.Join(mcpgateway.NativeGroups, ","))
	cmd.Flags().StringArrayVar(&o.include, "include", nil, "expose only upstream names matching this pattern (repeatable)")
	cmd.Flags().StringArrayVar(&o.exclude, "exclude", nil, "hide upstream names matching this pattern (repeatable)")
	cmd.Flags().StringVar(&o.instance, "instance", "", "pin an instance ID or mi1 handle")
	return cmd
}

func parseNativeGroups(value string) ([]string, error) {
	switch strings.TrimSpace(value) {
	case "all":
		return nil, nil
	case "none":
		return []string{}, nil
	}
	groups := []string{}
	for _, g := range strings.Split(value, ",") {
		g = strings.TrimSpace(g)
		if !slices.Contains(mcpgateway.NativeGroups, g) {
			return nil, fmt.Errorf("unknown native group %q (want all, none, or %s)", g, strings.Join(mcpgateway.NativeGroups, ", "))
		}
		if !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	return groups, nil
}

func (a *application) runMCPProfileAdd(cmd *cobra.Command, o mcpProfileAddOptions, args []string) error {
	const operation = "mcp.profile.add"
	name := args[0]
	if err := mcpgateway.ValidateProfileName(name); err != nil {
		return agentapi.Usage(operation, err.Error())
	}
	var native []string
	nativeSet := cmd.Flags().Changed("native")
	if nativeSet {
		var err error
		if native, err = parseNativeGroups(o.native); err != nil {
			return agentapi.Usage(operation, err.Error())
		}
	}
	var sel *mcpgateway.Selection
	if len(args) == 2 {
		resolved, err := a.resolveProfileSelection(cmd.Context(), operation, args[1], o)
		if err != nil {
			return err
		}
		sel = &resolved
	} else if o.alias != "" || o.instance != "" || len(o.include)+len(o.exclude) > 0 {
		return agentapi.Usage(operation, "--alias, --include, --exclude and --instance need a HOST/SERVICE argument.")
	}
	store, err := profileStore()
	if err != nil {
		return err
	}
	before, err := store.Load()
	if err != nil {
		return profileUsage(operation, err)
	}
	file, err := store.Update(cmd.Context(), o.expected(cmd), func(f *mcpgateway.File) error {
		return addToProfile(f, name, nativeSet, native, sel)
	})
	if err != nil {
		return profileUsage(operation, err)
	}
	return a.writeProfiles(cmd, o.asJSON, profilesData(store, file, []string{name}, file.Revision != before.Revision))
}

func addToProfile(f *mcpgateway.File, name string, nativeSet bool, native []string, sel *mcpgateway.Selection) error {
	p, exists := f.Profiles[name]
	if !exists {
		p = mcpgateway.Profile{Services: []mcpgateway.Selection{}}
	}
	if nativeSet {
		p.Native = native
	}
	if sel != nil {
		var err error
		if p.Services, err = upsertSelection(p.Services, *sel); err != nil {
			return err
		}
	}
	f.Profiles[name] = p
	return nil
}

// upsertSelection updates an entry for the same service and alias, and
// refuses to give an alias or service a second entry.
func upsertSelection(services []mcpgateway.Selection, sel mcpgateway.Selection) ([]mcpgateway.Selection, error) {
	for i, s := range services {
		sameService := s.HostID == sel.HostID && s.ServiceID == sel.ServiceID
		switch {
		case sameService && s.Alias == sel.Alias:
			services[i] = sel
			return services, nil
		case sameService:
			return nil, fmt.Errorf("%w: that service is already selected as %s; rename it with mesh mcp profile alias", mcpgateway.ErrInvalid, s.Alias)
		case s.Alias == sel.Alias:
			return nil, fmt.Errorf("%w: alias %s already names another service; remove it first", mcpgateway.ErrInvalid, sel.Alias)
		}
	}
	return append(services, sel), nil
}

// resolveProfileSelection binds HOST/SERVICE to stable IDs through the
// host's passive catalog.
func (a *application) resolveProfileSelection(ctx context.Context, operation, ref string, o mcpProfileAddOptions) (mcpgateway.Selection, error) {
	hostArg, serviceRef, err := parseQualifiedMCPServiceRef(ref, operation)
	if err != nil {
		return mcpgateway.Selection{}, err
	}
	view, host, err := a.findCatalogService(ctx, operation, hostArg, o.identityName, func(v mcphost.CatalogService) bool {
		return v.Name == serviceRef || v.ID == serviceRef
	})
	if err != nil {
		return mcpgateway.Selection{}, err
	}
	alias := o.alias
	if alias == "" {
		alias = view.Name
	}
	instance := ""
	if o.instance != "" {
		if instance, err = resolveMCPInstanceArg(operation, o.instance, host.ID); err != nil {
			return mcpgateway.Selection{}, err
		}
	}
	return mcpgateway.Selection{Alias: alias, HostID: host.ID, ServiceID: view.ID, ServiceName: view.Name, Instance: instance,
		Include: nonNil(o.include), Exclude: nonNil(o.exclude)}, nil
}

func (a *application) findCatalogService(ctx context.Context, operation, hostArg, identityName string, match func(mcphost.CatalogService) bool) (mcphost.CatalogService, HostRecord, error) {
	caller, err := profileCatalogCaller(identityName)
	if err != nil {
		return mcphost.CatalogService{}, HostRecord{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*mcpProfileTimeout)
	defer cancel()
	host, err := resolveMCPTargetHost(ctx, hostArg, a.dependencies.DialPassive)
	if err != nil {
		return mcphost.CatalogService{}, HostRecord{}, err
	}
	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()
	views, err := fetchMCPCatalogViews(ctx, pool, host, caller)
	if err != nil {
		return mcphost.CatalogService{}, host, agentapi.Fail(agentError(err, operation, hostArg, agentapi.HostUnreachable).Code, operation, hostArg, err.Error())
	}
	for _, v := range views {
		if match(v) {
			return v, host, nil
		}
	}
	return mcphost.CatalogService{}, host, agentapi.Fail(agentapi.ServiceNotFound, operation, hostArg, "No matching hosted MCP server on "+hostArg+".")
}

func (a *application) mcpProfileRemoveCommand() *cobra.Command {
	var flags mcpProfileFlags
	cmd := &cobra.Command{Use: "rm NAME [ALIAS]", Short: "Remove a profile, or one service from it", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		const operation = "mcp.profile.remove"
		store, err := profileStore()
		if err != nil {
			return err
		}
		file, err := store.Update(cmd.Context(), flags.expected(cmd), func(f *mcpgateway.File) error {
			return removeFromProfile(f, args)
		})
		if err != nil {
			return profileUsage(operation, err)
		}
		names := []string{}
		if len(args) == 2 {
			names = append(names, args[0])
		}
		return a.writeProfiles(cmd, flags.asJSON, profilesData(store, file, names, true))
	}}
	flags.bind(cmd, false)
	return cmd
}

func removeFromProfile(f *mcpgateway.File, args []string) error {
	p, ok := f.Profiles[args[0]]
	if !ok {
		return fmt.Errorf("%w: no profile named %s", mcpgateway.ErrNotFound, args[0])
	}
	if len(args) == 1 {
		delete(f.Profiles, args[0])
		return nil
	}
	index := slices.IndexFunc(p.Services, func(s mcpgateway.Selection) bool { return s.Alias == args[1] })
	if index < 0 {
		return fmt.Errorf("%w: profile %s has no service %s", mcpgateway.ErrNotFound, args[0], args[1])
	}
	p.Services = slices.Delete(p.Services, index, index+1)
	f.Profiles[args[0]] = p
	return nil
}

func (a *application) mcpProfileAliasCommand() *cobra.Command {
	var flags mcpProfileFlags
	cmd := &cobra.Command{Use: "alias NAME OLD NEW", Short: "Rename a service's alias; exposed names do not change", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		const operation = "mcp.profile.alias"
		store, err := profileStore()
		if err != nil {
			return err
		}
		file, err := store.Update(cmd.Context(), flags.expected(cmd), func(f *mcpgateway.File) error {
			return renameAlias(f, args[0], args[1], args[2])
		})
		if err != nil {
			return profileUsage(operation, err)
		}
		return a.writeProfiles(cmd, flags.asJSON, profilesData(store, file, []string{args[0]}, true))
	}}
	flags.bind(cmd, false)
	return cmd
}

func renameAlias(f *mcpgateway.File, name, old, next string) error {
	p, ok := f.Profiles[name]
	if !ok {
		return fmt.Errorf("%w: no profile named %s", mcpgateway.ErrNotFound, name)
	}
	index := slices.IndexFunc(p.Services, func(s mcpgateway.Selection) bool { return s.Alias == old })
	if index < 0 {
		return fmt.Errorf("%w: profile %s has no service %s", mcpgateway.ErrNotFound, name, old)
	}
	p.Services[index].Alias = next
	f.Profiles[name] = p
	return nil
}

func (a *application) mcpProfileRepairCommand() *cobra.Command {
	var flags mcpProfileFlags
	var confirm bool
	cmd := &cobra.Command{Use: "repair NAME ALIAS", Short: "Rebind an entry whose service was removed and published again, by name", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return a.runMCPProfileRepair(cmd, flags, confirm, args[0], args[1])
	}}
	flags.bind(cmd, true)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "bind the entry to the service now published under its recorded name")
	return cmd
}

func (a *application) runMCPProfileRepair(cmd *cobra.Command, flags mcpProfileFlags, confirm bool, name, alias string) error {
	const operation = "mcp.profile.repair"
	store, err := profileStore()
	if err != nil {
		return err
	}
	file, err := store.Load()
	if err != nil {
		return profileUsage(operation, err)
	}
	p, ok := file.Profiles[name]
	index := slices.IndexFunc(p.Services, func(s mcpgateway.Selection) bool { return s.Alias == alias })
	if !ok || index < 0 {
		return agentapi.Usage(operation, fmt.Sprintf("Profile %s has no service %s.", name, alias))
	}
	entry := p.Services[index]
	if entry.ServiceName == "" {
		return agentapi.Usage(operation, "The entry has no recorded service name; remove it and add the service again.")
	}
	view, _, err := a.findCatalogService(cmd.Context(), operation, entry.HostID, flags.identityName, func(v mcphost.CatalogService) bool { return v.Name == entry.ServiceName })
	if err != nil {
		return err
	}
	repair := agentapi.MCPProfileRepair{Profile: name, Alias: alias, HostID: entry.HostID, ServiceName: entry.ServiceName,
		OldServiceID: entry.ServiceID, NewServiceID: view.ID, Revision: file.Revision}
	if confirm && view.ID != entry.ServiceID {
		expected := flags.expected(cmd)
		if expected == nil {
			expected = &file.Revision
		}
		updated, err := store.Update(cmd.Context(), expected, func(f *mcpgateway.File) error {
			current := f.Profiles[name]
			current.Services[index].ServiceID, current.Services[index].Instance = view.ID, ""
			f.Profiles[name] = current
			return nil
		})
		if err != nil {
			return profileUsage(operation, err)
		}
		repair.Confirmed, repair.Revision = true, updated.Revision
	}
	return a.writeRepair(cmd, flags.asJSON, confirm, repair)
}

func (a *application) writeRepair(cmd *cobra.Command, asJSON, confirm bool, repair agentapi.MCPProfileRepair) error {
	if repair.OldServiceID != repair.NewServiceID && !confirm {
		message := fmt.Sprintf("Service %s on %s is now %s (was %s). Run again with --confirm to rebind %s; its exposed names change.",
			repair.ServiceName, repair.HostID, repair.NewServiceID, repair.OldServiceID, repair.Alias)
		if asJSON {
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusError, Data: repair,
				Errors: []agentapi.Error{agentapi.Usage("mcp.profile.repair", message).Err}})
		}
		return agentapi.Usage("mcp.profile.repair", message)
	}
	if asJSON {
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: repair})
	}
	text := fmt.Sprintf("%s already points at %s.\n", repair.Alias, repair.NewServiceID)
	if repair.Confirmed {
		text = fmt.Sprintf("Rebound %s to %s (revision %d).\n", repair.Alias, repair.NewServiceID, repair.Revision)
	}
	_, err := io.WriteString(cmd.OutOrStdout(), text)
	return executionBoundaryError("print repair", err)
}
