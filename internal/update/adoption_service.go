package update

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

type adoptionServiceOutput struct{ data []byte }

func (w *adoptionServiceOutput) Write(data []byte) (int, error) {
	if len(data) > (1<<20)-len(w.data) {
		return 0, errors.New("update: daemon service definition exceeds 1 MiB")
	}
	w.data = append(w.data, data...)
	return len(data), nil
}

func adoptionServiceDigest(ctx context.Context, settings updateinstall.Settings) (string, error) {
	service, err := updateinstall.NewSystemService(settings.Service)
	if err != nil {
		return "", hostError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := service.Preflight(ctx); err != nil {
		return "", hostError(err)
	}
	var command *exec.Cmd
	if settings.Service.Kind == "systemd" {
		command = exec.CommandContext(ctx, "systemctl", "--user", "cat", "--no-pager")
		command.Args = append(command.Args, settings.Service.Name)
	} else {
		command = exec.CommandContext(ctx, "plutil", "-convert", "xml1", "-o", "-")
		command.Args = append(command.Args, settings.Service.ConfigPath)
	}
	var output adoptionServiceOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("update: inspect existing daemon service definition: %w", err)
	}
	if len(output.data) == 0 {
		return "", errors.New("update: daemon service definition is empty")
	}
	return discoveryDigest(output.data), nil
}
