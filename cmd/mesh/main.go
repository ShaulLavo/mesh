// Command mesh runs the Mesh client, daemon, and detached session workers.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/fang"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
)

func main() {
	if err := initializeDeployment(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	root := cli.NewCommand(commandDependencies())
	if plainAgentCommand(os.Args[1:]) {
		finishAgentCommand(os.Args[1], root.ExecuteContext(context.Background()))
		return
	}
	options := []fang.Option{
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithErrorHandler(func(output io.Writer, styles fang.Styles, err error) {
			if _, ok := cli.StatusCode(err); ok {
				return
			}
			cli.RenderError(output, styles, cli.PrivacyErrorForArguments(err, os.Args[1:]))
		}),
	}
	// Without this a tagged build reports itself as built from source.
	if version := release.Metadata().Version; version != "" {
		options = append(options, fang.WithVersion(version))
	}
	err := fang.Execute(context.Background(), root.Command, options...)
	if err == nil {
		return
	}
	if status, ok := cli.StatusCode(err); ok {
		os.Exit(status)
	}
	os.Exit(1)
}

func plainAgentCommand(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	switch arguments[0] {
	// session-worker sets its own signal policy; Fang's NotifyContext would
	// silently decide which signals end a session.
	case "session-worker", "agent", "agent-hook", "agent-resume", "version", "update", "update-helper", "update-notice-check", "update-bootstrap", "update-bootstrap-status":
		return true
	default:
		return false
	}
}

func finishAgentCommand(name string, err error) {
	err = cli.PrivacyErrorForArguments(err, os.Args[1:])
	if err == nil || name == "agent-hook" {
		return
	}
	if status, ok := cli.StatusCode(err); ok {
		os.Exit(status)
	}
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func entryCommand(args []string) string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "--leave-key" {
			if len(args) < 2 {
				return ""
			}
			args = args[2:]
			continue
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func namingCommand(args []string) bool {
	command := entryCommand(args)
	return command != "device" && !plainAgentCommand([]string{command})
}

func deploymentRequested(args []string) bool {
	switch entryCommand(args) {
	case "app", "serve", "unserve", "private-names":
		return true
	case "daemon":
		for _, arg := range args {
			if arg == "--https-port" || arg == "--edge" || arg == "--private-names-config" || strings.HasPrefix(arg, "--https-port=") || strings.HasPrefix(arg, "--edge=") || strings.HasPrefix(arg, "--private-names-config=") {
				return true
			}
		}
	}
	return false
}

func initializeDeployment(args []string) error {
	if !namingCommand(args) {
		return nil
	}
	config, err := cli.ConfigPath()
	if err != nil {
		return fmt.Errorf("deployment configuration: %w", err)
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return fmt.Errorf("deployment state: %w", err)
	}
	path := filepath.Join(filepath.Dir(config), "domains.json")
	var policyErr error
	switch entryCommand(args) {
	case "ls", "list":
		policyErr = domainpolicy.InitializeReadOnlyDeployment(path, state)
	default:
		policyErr = domainpolicy.InitializeDeployment(path, state, deploymentRequested(args))
	}
	if policyErr != nil {
		return fmt.Errorf("initialize deployment policy: %w", policyErr)
	}
	return nil
}
