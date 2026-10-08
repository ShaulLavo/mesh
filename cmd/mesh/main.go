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
	"github.com/shaul/mesh/internal/release"
)

func main() {
	if namingCommand(os.Args[1:]) {
		config, err := cli.ConfigPath()
		if err == nil {
			err = domainpolicy.Initialize(filepath.Join(filepath.Dir(config), "domains.json"))
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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

func namingCommand(args []string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "--leave-key" {
			if len(args) < 2 {
				return false
			}
			args = args[2:]
			continue
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "daemon", "serve", "unserve", "app", "private-names":
		return true
	default:
		return false
	}
}
