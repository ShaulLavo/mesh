// Command mesh runs the Mesh client, daemon, and detached session workers.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/charmbracelet/fang"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
)

func main() {
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
			cli.RenderError(output, styles, err)
		}),
	}
	// Without this a tagged build reports itself as built from source.
	if version := bootstrap.Version(); version != "" {
		options = append(options, fang.WithVersion(version))
	}
	err := fang.Execute(context.Background(), root, options...)
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
	case "agent", "agent-hook", "agent-resume":
		return true
	default:
		return false
	}
}

func finishAgentCommand(name string, err error) {
	if err == nil || name == "agent-hook" {
		return
	}
	if status, ok := cli.StatusCode(err); ok {
		os.Exit(status)
	}
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
