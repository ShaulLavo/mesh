package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type cacheEnvironment struct {
	name string
	vars map[string]string
}

func cacheEnvironments(home string) []cacheEnvironment {
	return []cacheEnvironment{
		{name: "defaults", vars: map[string]string{"HOME": home}},
		{name: "xdg set", vars: map[string]string{"HOME": home, "XDG_CACHE_HOME": "/xdg/cache"}},
		{name: "xdg empty", vars: map[string]string{"HOME": home, "XDG_CACHE_HOME": ""}},
		{name: "override", vars: map[string]string{"HOME": home, "MESH_UPDATE_CACHE_DIR": "/custom/updates"}},
		{name: "override and xdg", vars: map[string]string{"HOME": home, "XDG_CACHE_HOME": "/xdg/cache", "MESH_UPDATE_CACHE_DIR": "/custom/updates"}},
		{name: "override empty", vars: map[string]string{"HOME": home, "XDG_CACHE_HOME": "/xdg/cache", "MESH_UPDATE_CACHE_DIR": ""}},
	}
}

// testShells is /bin/sh plus dash when it is installed, because remote
// accounts run these scripts under whatever sh they have.
func testShells(t *testing.T) []string {
	t.Helper()
	shells := []string{"/bin/sh"}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}
	return shells
}

func runCacheShell(t *testing.T, shell, uname string, vars map[string]string) string {
	t.Helper()
	bin := t.TempDir()
	stub := "#!/bin/sh\necho " + uname + "\n"
	if err := os.WriteFile(filepath.Join(bin, "uname"), []byte(stub), 0o700); err != nil { //nolint:gosec // isolated tool stub
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin}
	for key, value := range vars {
		env = append(env, key+"="+value)
	}
	command := exec.CommandContext(t.Context(), shell, "-c", UpdateCacheShell+`printf '%s' "$cache"`) //nolint:gosec // fixed snippet under test
	command.Env = env
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%s: %v", shell, err)
	}
	return string(output)
}

func TestUpdateCacheShellMatchesGo(t *testing.T) {
	platforms := []struct{ goos, uname string }{{"darwin", "Darwin"}, {"linux", "Linux"}}
	for _, shell := range testShells(t) {
		for _, platform := range platforms {
			for _, environment := range cacheEnvironments("/home/fixture") {
				t.Run(filepath.Base(shell)+"/"+platform.goos+"/"+environment.name, func(t *testing.T) {
					want, err := UpdateCacheDir(platform.goos, func(key string) string { return environment.vars[key] })
					if err != nil {
						t.Fatal(err)
					}
					if got := runCacheShell(t, shell, platform.uname, environment.vars); got != want {
						t.Fatalf("shell rule = %q, Go rule = %q", got, want)
					}
				})
			}
		}
	}
}

func TestUpdateCacheDirFollowsUserCacheDir(t *testing.T) {
	for _, environment := range cacheEnvironments(t.TempDir()) {
		t.Run(environment.name, func(t *testing.T) {
			for _, key := range []string{"HOME", "XDG_CACHE_HOME", "MESH_UPDATE_CACHE_DIR"} {
				t.Setenv(key, environment.vars[key])
			}
			got, err := UpdateCacheDir(runtime.GOOS, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			if override := environment.vars["MESH_UPDATE_CACHE_DIR"]; override != "" {
				if got != override {
					t.Fatalf("override ignored: %q", got)
				}
				return
			}
			base, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			if got != filepath.Join(base, "mesh", "updates") {
				t.Fatalf("update cache %q is not under the daemon's cache %q", got, base)
			}
		})
	}
}

func TestUpdateCacheDirRefusesUnusableEnvironments(t *testing.T) {
	cases := []struct {
		goos string
		vars map[string]string
		want string
	}{
		{"darwin", map[string]string{}, "$HOME is not defined"},
		{"linux", map[string]string{}, "neither $XDG_CACHE_HOME nor $HOME"},
		{"linux", map[string]string{"HOME": "/home/fixture", "XDG_CACHE_HOME": "relative"}, "relative"},
	}
	for _, c := range cases {
		_, err := UpdateCacheDir(c.goos, func(key string) string { return c.vars[key] })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %v: error %v, want %q", c.goos, c.vars, err, c.want)
		}
	}
}
