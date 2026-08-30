// Command mesh runs the Mesh client, daemon, and detached session workers.
package main

import (
	"context"
	"io"
	"os"
	"syscall"

	"github.com/charmbracelet/fang"

	"github.com/shaul/mesh/internal/cli"
)

func main() {
	root := cli.NewCommand(cli.Dependencies{})
	err := fang.Execute(
		context.Background(),
		root,
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithErrorHandler(func(output io.Writer, styles fang.Styles, err error) {
			if _, ok := cli.StatusCode(err); ok {
				return
			}
			fang.DefaultErrorHandler(output, styles, err)
		}),
	)
	if err == nil {
		return
	}
	if status, ok := cli.StatusCode(err); ok {
		os.Exit(status)
	}
	os.Exit(1)
}
