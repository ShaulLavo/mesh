package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/skills"
	"github.com/spf13/cobra"
)

const (
	skillInstallVerb   = "install"
	skillUpdateVerb    = "update"
	skillStatusVerb    = "status"
	skillUninstallVerb = "uninstall"
	skillShowVerb      = "show"
)

func (a *application) skillCommand() *cobra.Command {
	group := &cobra.Command{Use: "skill", Short: "Show and manage the embedded Mesh agent skill", RunE: func(_ *cobra.Command, _ []string) error {
		return agentapi.Usage("skill.status", "Choose mesh skill show, install, update, status or uninstall.")
	}}
	group.Flags().Bool("json", false, "write one agent result")
	group.AddCommand(skillShowCommand())
	for _, action := range []string{skillInstallVerb, skillUpdateVerb, skillStatusVerb, skillUninstallVerb} {
		group.AddCommand(a.skillManagedCommand(action))
	}
	return group
}
func skillShowCommand() *cobra.Command {
	var file string
	command := &cobra.Command{Use: skillShowVerb, Short: "Print exact embedded skill bytes without reading local configuration", Args: profileArgs("skill.show"), RunE: func(cmd *cobra.Command, _ []string) error {
		data, err := skills.Show(file)
		if err != nil {
			return agentapi.Usage("skill.show", err.Error())
		}
		if _, err := cmd.OutOrStdout().Write(data); err != nil {
			return fmt.Errorf("print embedded skill: %w", err)
		}
		return nil
	}}
	command.Flags().StringVar(&file, "file", "", "embedded file, such as references/commands.md")
	return command
}
func (a *application) skillManagedCommand(action string) *cobra.Command {
	var root string
	var asJSON bool
	operation := "skill." + action
	command := &cobra.Command{Use: action, Short: skillSummary(action), Args: profileArgs(operation), RunE: func(cmd *cobra.Command, _ []string) error {
		return a.runSkillManaged(cmd, action, root, asJSON)
	}}
	command.Flags().StringVar(&root, "dir", "", "existing skills root; omit for all registered roots on status or update")
	command.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return command
}
func skillSummary(action string) string {
	switch action {
	case skillInstallVerb:
		return "Install the skill into an explicitly selected skills root"
	case skillUpdateVerb:
		return "Update managed roots and preserve user-modified files"
	case skillUninstallVerb:
		return "Remove unchanged managed files and unregister one root"
	default:
		return "Inspect bundle files and registration without changing them"
	}
}
func skillFailure(err error, operation, target string) error {
	code := agentapi.StateUnavailable
	var problem *skills.Problem
	if errors.As(err, &problem) {
		switch problem.Kind {
		case "unsafe":
			code = agentapi.SkillUnsafePath
		case "conflict":
			code = agentapi.SkillConflict
		}
	}
	return agentapi.Fail(code, operation, target, err.Error())
}
func skillResult(roots []skills.Root, operation string) (agentapi.SkillResult, []agentapi.Error) {
	result := agentapi.SkillResult{Roots: make([]agentapi.SkillRoot, 0, len(roots))}
	diagnostics := []agentapi.Error{}
	for _, root := range roots {
		row := agentapi.SkillRoot{Path: root.Path, State: root.State, Registered: root.Registered, RegistrationState: root.RegistrationState, MeshVersion: root.MeshVersion, Bundle: root.Bundle, ModifiedFiles: root.ModifiedFiles, MissingFiles: root.MissingFiles, ProposedFiles: root.ProposedFiles, RetainedFiles: root.RetainedFiles, Problems: []agentapi.Error{}}
		for _, problem := range root.Problems {
			entry := agentError(skillFailure(problem, operation, root.Path), operation, root.Path, agentapi.StateUnavailable)
			row.Problems = append(row.Problems, entry)
			diagnostics = append(diagnostics, entry)
		}
		result.Roots = append(result.Roots, row)
	}
	return result, diagnostics
}
func printSkillResult(cmd *cobra.Command, result agentapi.SkillResult, diagnostics []agentapi.Error) error {
	for _, root := range result.Roots {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s)\n", root.Path, root.State, root.RegistrationState); err != nil {
			return fmt.Errorf("print skill status: %w", err)
		}
		for _, path := range root.ProposedFiles {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Proposed replacement:", path); err != nil {
				return fmt.Errorf("print proposed path: %w", err)
			}
		}
		if len(root.RetainedFiles) > 0 {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Retained:", strings.Join(root.RetainedFiles, ", ")); err != nil {
				return fmt.Errorf("print retained paths: %w", err)
			}
		}
	}
	if len(diagnostics) > 0 {
		return agentapi.Fail(diagnostics[0].Code, diagnostics[0].Operation, diagnostics[0].Target, diagnostics[0].Message)
	}
	return nil
}

func (a *application) runSkillManaged(cmd *cobra.Command, action, root string, asJSON bool) error {
	operation := "skill." + action
	if (action == skillInstallVerb || action == skillUninstallVerb) && root == "" {
		return agentapi.Usage(operation, "--dir ROOT is required.")
	}
	roots, err := loadSkillRoots(cmd, action, root)
	if err != nil {
		return skillFailure(err, operation, root)
	}

	data, diagnostics := skillResult(roots, operation)
	if asJSON {
		status := agentapi.StatusOK
		if len(diagnostics) > 0 {
			status = agentapi.StatusPartial
		}
		if len(diagnostics) > 0 && !skillHasData(data) {
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusError, Errors: diagnostics})
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: status, Data: data, Errors: diagnostics})
	}
	return printSkillResult(cmd, data, diagnostics)
}

func loadSkillRoots(cmd *cobra.Command, action, root string) ([]skills.Root, error) {
	operation := "skill." + action
	config, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate skill state: %w", err)
	}
	manager := skills.Manager{ConfigDir: filepath.Dir(config), StateDir: state, MeshVersion: release.Metadata().Version}
	var roots []skills.Root
	if action == skillStatusVerb {
		roots, err = manager.Status(root)
	} else {
		roots, err = manager.Apply(cmd.Context(), action, root)
	}
	if err != nil {
		return nil, skillFailure(err, operation, root)
	}
	return roots, nil
}

func skillHasData(data agentapi.SkillResult) bool {
	for _, root := range data.Roots {
		if root.State != "" {
			return true
		}
	}
	return false
}
