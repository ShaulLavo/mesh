// Command mesh runs the Mesh client, daemon, and detached session workers.
package main

import (
	"context"
	"io"
	"os"
	"syscall"

	"github.com/charmbracelet/fang"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
)

func main() {
	root := cli.NewCommand(commandDependencies())
	options := []fang.Option{
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithErrorHandler(func(output io.Writer, styles fang.Styles, err error) {
			if _, ok := cli.StatusCode(err); ok {
				return
			}
			fang.DefaultErrorHandler(output, styles, err)
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
