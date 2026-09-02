package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func lookup(found ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, name := range found {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestLocalTailscaleStepsPlansPerPlatform(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		goos  string
		have  []string
		want  []string
		fails string
	}{
		{
			name: "macOS installs through Homebrew then logs in",
			goos: "darwin", have: []string{"brew"},
			want: []string{"/usr/bin/brew install tailscale", "sudo /usr/bin/brew services start tailscale", "tailscale up"},
		},
		{
			name: "an installed Tailscale only needs the login",
			goos: "darwin", have: []string{"tailscale", "brew"},
			want: []string{"tailscale up"},
		},
		{
			name: "Arch installs through pacman",
			goos: "linux", have: []string{"pacman"},
			want: []string{"sudo /usr/bin/pacman -S --needed --noconfirm tailscale", "sudo systemctl enable --now tailscaled", "tailscale up"},
		},
		{
			name: "macOS without Homebrew says so instead of guessing",
			goos: "darwin", have: nil, fails: "Homebrew is required",
		},
		{
			name: "an unsupported platform names itself",
			goos: "windows", have: nil, fails: "not supported here",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			steps, err := localTailscaleSteps(row.goos, lookup(row.have...))
			if row.fails != "" {
				if err == nil || !strings.Contains(err.Error(), row.fails) {
					t.Fatalf("error = %v, want it to mention %q", err, row.fails)
				}
				return
			}
			if err != nil {
				t.Fatalf("localTailscaleSteps() error = %v", err)
			}
			var got []string
			for _, step := range steps {
				got = append(got, step.display())
			}
			if strings.Join(got, " | ") != strings.Join(row.want, " | ") {
				t.Fatalf("steps = %q, want %q", got, row.want)
			}
		})
	}
}

func TestLocalStepDisplayIsWhatWouldBeTyped(t *testing.T) {
	t.Parallel()

	step := localStep{"x", "sudo", []string{"/usr/bin/brew", "services", "start", "tailscale"}}
	if step.display() != "sudo /usr/bin/brew services start tailscale" {
		t.Fatalf("display() = %q", step.display())
	}
	if !errors.Is(exec.ErrNotFound, exec.ErrNotFound) {
		t.Fatal("sanity")
	}
}
