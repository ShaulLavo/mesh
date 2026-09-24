package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/agentresume"
	"github.com/shaul/mesh/internal/procmem"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

const defaultGCIdle = 6 * time.Hour

type gcAction string

const (
	gcHibernate gcAction = "hibernate"
	gcKill      gcAction = "kill"
	gcLeave     gcAction = "left running"
)

// gcPolicy is everything a gc decision depends on, so planning is a pure
// function of the catalog.
type gcPolicy struct {
	now    time.Time
	idle   time.Duration
	shells bool
	// containing are the sessions whose terminals show this process. A
	// dropped connection leaves them detached with gc still running inside.
	containing []protocol.SessionIdentity
}

// gcEntry is one idle session and what gc would do with it.
type gcEntry struct {
	host   HostRecord
	local  bool
	row    protocol.SessionInfo
	idle   time.Duration
	action gcAction
	notes  []string
}

func (e gcEntry) label() string {
	return e.row.ID + " on " + e.host.Alias
}

func (a *application) gcCommand() *cobra.Command {
	var (
		idle   time.Duration
		shells bool
		yes    bool
	)
	command := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim memory from sessions left detached and idle",
		Long: "gc looks for sessions on this host and on known hosts that nobody is\n" +
			"attached to and that have printed nothing for --idle. Claude and Codex\n" +
			"sessions hibernate: their agent stops and resumes the same conversation\n" +
			"on the next attach. Plain shells are listed but left running unless\n" +
			"--shells is given, which ends them.\n\n" +
			"Without --yes, gc prints its plan and changes nothing. Attached sessions\n" +
			"and the session gc itself runs in are never touched.",
		Example: "# Show what would be reclaimed\n" +
			"mesh gc\n" +
			"# Hibernate agents detached and quiet for a day\n" +
			"mesh gc --idle 24h --yes\n" +
			"# Also end idle plain shells\n" +
			"mesh gc --shells --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if idle <= 0 {
				return errors.New("--idle must be a positive duration, such as 6h")
			}
			return a.runGC(cmd, idle, shells, yes)
		},
	}
	command.Flags().DurationVar(&idle, "idle", defaultGCIdle, "minimum time a session has been detached and silent")
	command.Flags().BoolVar(&shells, "shells", false, "also end idle sessions that have no resumable agent")
	command.Flags().BoolVar(&yes, "yes", false, "act on the plan instead of only printing it")
	return command
}

func (a *application) runGC(cmd *cobra.Command, idle time.Duration, shells, yes bool) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	catalog, err := a.gcCatalog(cmd, hosts)
	if err != nil {
		return err
	}
	policy := gcPolicy{now: a.dependencies.Now(), idle: idle, shells: shells, containing: a.dependencies.Containment(cmd.Context())}
	entries := planGC(policy, catalog)
	output := cmd.OutOrStdout()
	if len(entries) == 0 {
		_, err := fmt.Fprintf(output, "nothing to reclaim: no detached session has been idle for %s\n", idle)
		return err
	}
	if err := writeGCPlan(output, entries); err != nil {
		return err
	}
	if !yes {
		return writeGCSummary(output, entries, shells)
	}
	return a.applyGCPlan(cmd, policy, entries)
}

// gcCatalog is the ls catalog minus rows that came from cache: a stale row
// describes a host gc cannot reach, and acting on it would only fail.
func (a *application) gcCatalog(cmd *cobra.Command, hosts []HostRecord) ([]HostSessions, error) {
	var catalog []HostSessions
	local, err := localSessionRowsMeasured(procmem.Snapshot())
	if err != nil {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "this host: local sessions unavailable: %s\n", safeRemoteText(err.Error())); err != nil {
			return nil, err
		}
	} else {
		catalog = append(catalog, HostSessions{Local: true, Host: HostRecord{Alias: localHostAlias}, Sessions: local})
	}
	if len(hosts) == 0 {
		return catalog, nil
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return nil, err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	results, err := CollectHostSessions(cmd.Context(), hosts, defaultCatalogTimeout, a.queryHost, cache)
	if err != nil {
		return nil, err
	}
	for _, result := range results {
		if result.Err == nil && !result.Stale {
			catalog = append(catalog, result)
			continue
		}
		reason := "no live catalog"
		if result.Err != nil {
			reason = safeRemoteText(result.Err.Error())
		}
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s: unavailable: %s; its sessions were not considered\n", result.Host.Alias, reason); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

// planGC lists every detached session idle for at least policy.idle, with
// the action gc would take. A session reachable through two host entries is
// planned once, local first.
func planGC(policy gcPolicy, catalog []HostSessions) []gcEntry {
	var entries []gcEntry
	seen := make(map[protocol.SessionIdentity]bool)
	for _, result := range catalog {
		if result.Stale || result.Err != nil {
			continue
		}
		for _, row := range result.Sessions {
			key := gcIdentity(result, row)
			if seen[key] {
				continue
			}
			seen[key] = true
			entry, ok := planGCRow(policy, result, row, key)
			if !ok {
				continue
			}
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].row.MemoryBytes != entries[j].row.MemoryBytes {
			return entries[i].row.MemoryBytes > entries[j].row.MemoryBytes
		}
		if entries[i].host.Alias != entries[j].host.Alias {
			return entries[i].host.Alias < entries[j].host.Alias
		}
		return entries[i].row.ID < entries[j].row.ID
	})
	return entries
}

func gcIdentity(result HostSessions, row protocol.SessionInfo) protocol.SessionIdentity {
	hostID := row.HostID
	if hostID == "" {
		hostID = result.Host.ID
	}
	return protocol.SessionIdentity{HostID: hostID, SessionID: row.ID}
}

func planGCRow(policy gcPolicy, result HostSessions, row protocol.SessionInfo, key protocol.SessionIdentity) (gcEntry, bool) {
	// Running means attached. Only a session nobody is looking at qualifies.
	if row.State != worker.StateDetached {
		return gcEntry{}, false
	}
	since, fallback := gcDetachedSince(row)
	quiet := policy.now.Sub(lastOutputAt(row))
	detached := policy.now.Sub(since)
	if quiet < policy.idle || detached < policy.idle {
		return gcEntry{}, false
	}
	entry := gcEntry{host: result.Host, local: result.Local, row: row, idle: min(quiet, detached)}
	switch {
	case slices.Contains(policy.containing, key):
		entry.action = gcLeave
		entry.notes = append(entry.notes, "contains this terminal")
	case gcHibernatable(row):
		entry.action = gcHibernate
	case policy.shells:
		entry.action = gcKill
	default:
		entry.action = gcLeave
		entry.notes = append(entry.notes, "plain shell")
	}
	if fallback != "" {
		entry.notes = append(entry.notes, fallback)
	}
	return entry, true
}

// gcDetachedSince prefers the worker's detach time. Workers older than
// hibernation never wrote one; their last attach start is the closest bound,
// and the plan says so because it can be long before the actual detach.
func gcDetachedSince(row protocol.SessionInfo) (time.Time, string) {
	if row.DetachedAt != nil {
		return *row.DetachedAt, ""
	}
	if row.LastAttachedAt != nil {
		return *row.LastAttachedAt, "detach time unknown; idle counted from last attach"
	}
	return row.CreatedAt, "detach time unknown; idle counted from start"
}

// gcHibernatable mirrors the worker's own test: a registered conversation
// still open in this session. The worker re-checks and has the last word.
func gcHibernatable(row protocol.SessionInfo) bool {
	return row.Recovery != nil && row.Recovery.Agent != nil && row.Recovery.Agent.Lifecycle == agentresume.Active
}

func gcActionText(entry gcEntry) string {
	if len(entry.notes) == 0 {
		return string(entry.action)
	}
	return string(entry.action) + " (" + strings.Join(entry.notes, "; ") + ")"
}

func gcWhat(row protocol.SessionInfo) string {
	var what string
	if gcHibernatable(row) {
		what = string(row.Recovery.Agent.Provider)
	} else {
		what = strings.Join(row.Command, " ")
	}
	if row.Recovery != nil && strings.TrimSpace(row.Recovery.Title) != "" {
		what += " · " + row.Recovery.Title
	}
	return SafeTerminalText(what)
}

func writeGCPlan(output io.Writer, entries []gcEntry) error {
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "HOST\tID\tMEM\tIDLE\tACTION\tWHAT"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			SafeTerminalText(entry.host.Alias), entry.row.ID, formatBytes(entry.row.MemoryBytes),
			compactDuration(entry.idle), gcActionText(entry), gcWhat(entry.row),
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeGCSummary(output io.Writer, entries []gcEntry, shells bool) error {
	count, reclaimable, shellsLeft := 0, uint64(0), 0
	for _, entry := range entries {
		if entry.action == gcLeave {
			if slices.Contains(entry.notes, "plain shell") {
				shellsLeft++
			}
			continue
		}
		count++
		reclaimable += entry.row.MemoryBytes
	}
	if count == 0 {
		hint := ""
		if shellsLeft > 0 && !shells {
			hint = "; pass --shells to end idle plain shells"
		}
		_, err := fmt.Fprintf(output, "\nnothing to reclaim%s\n", hint)
		return err
	}
	_, err := fmt.Fprintf(output, "\nreclaimable: %s from %s; run mesh gc --yes to act\n", memoryTotal(reclaimable), sessionCount(count))
	return err
}

func memoryTotal(bytes uint64) string {
	if bytes == 0 {
		return "unmeasured memory"
	}
	return formatBytes(bytes)
}

func sessionCount(count int) string {
	if count == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", count)
}

// applyGCPlan acts on every planned session, keeps going past failures like
// rm, and reports each outcome.
func (a *application) applyGCPlan(cmd *cobra.Command, policy gcPolicy, entries []gcEntry) error {
	output := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(output); err != nil {
		return err
	}
	var (
		failures  []error
		reclaimed uint64
		count     int
	)
	for _, entry := range entries {
		if entry.action == gcLeave {
			continue
		}
		if err := a.applyGCEntry(cmd.Context(), policy, entry); err != nil {
			failures = append(failures, err)
			continue
		}
		count++
		reclaimed += entry.row.MemoryBytes
		verb := "hibernated"
		if entry.action == gcKill {
			verb = "killed"
		}
		if _, err := fmt.Fprintf(output, "%s %s\n", verb, SafeTerminalText(entry.label())); err != nil {
			return err
		}
	}
	if count > 0 {
		if _, err := fmt.Fprintf(output, "reclaimed %s from %s\n", memoryTotal(reclaimed), sessionCount(count)); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (a *application) applyGCEntry(parent context.Context, policy gcPolicy, entry gcEntry) error {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	if entry.action == gcHibernate {
		// The worker re-checks idle and detach against its own clocks, so a
		// session attached since the plan was printed is refused, not stopped.
		if !entry.local {
			return hibernateRemote(ctx, entry.host, a.dependencies.DialHost, entry.row.ID, policy.idle)
		}
		current, err := Find(entry.row.ID)
		if err != nil {
			return err
		}
		return hibernateLocal(current, policy.idle)
	}
	// Kill has no conditional form, so re-read the live session first. This
	// narrows the gap in which someone could attach to a moment.
	if err := a.confirmIdleShell(ctx, policy, entry); err != nil {
		return err
	}
	if !entry.local {
		return controlRemoteSession(ctx, entry.host, a.dependencies.DialHost, entry.row.ID, protocol.TypeKill, "")
	}
	current, err := Find(entry.row.ID)
	if err != nil {
		return err
	}
	return Kill(current)
}

func (a *application) confirmIdleShell(ctx context.Context, policy gcPolicy, entry gcEntry) error {
	var (
		inspection SessionInspection
		err        error
	)
	if entry.local {
		inspection, err = inspectLocalSession(ctx, PickerInspectRequest{HostAlias: localHostAlias, SessionID: entry.row.ID, PreviewCols: 1, PreviewRows: 1})
	} else {
		inspection, err = inspectRemoteSession(ctx, entry.host, a.dependencies.DialHost, entry.row.ID, 1, 1)
	}
	if err != nil {
		return fmt.Errorf("session %s: recheck before kill: %w", entry.label(), err)
	}
	if inspection.Attached {
		return fmt.Errorf("session %s: attached since the plan was made; left running", entry.label())
	}
	// Both times come from the host's clock, so skew with this machine
	// cannot make a busy shell look idle.
	if inspection.LastOutputAt != nil && inspection.ObservedAt.Sub(*inspection.LastOutputAt) < policy.idle {
		return fmt.Errorf("session %s: printed output since the plan was made; left running", entry.label())
	}
	return nil
}

// compactDuration renders an elapsed time in the same units as AGE.
func compactDuration(elapsed time.Duration) string {
	return ageAt(time.Time{}.Add(elapsed), time.Time{})
}
