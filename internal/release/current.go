package release

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"os"
	"runtime/debug"
)

// Version is set by the release linker. Development builds leave it empty.
var Version string

var executingBuild = readExecutingBuild()

// Current reports the executable image loaded by this process.
func Current() Build {
	build := executingBuild
	if Version != "" {
		build.Version = Version
	}
	return build
}

func readExecutingBuild() Build {
	build := Build{
		Platform:       CurrentPlatform(),
		StateVersion:   CurrentStateVersion,
		WorkerProtocol: CurrentWorkerProtocol,
		UpdateProtocol: CurrentUpdateProtocol,
	}
	path := executingExecutablePath()
	contents, err := os.ReadFile(path) //nolint:gosec // path names the executable loaded by this process
	if err != nil {
		build.Modified = true
		return build
	}
	digest := sha256.Sum256(contents)
	build.Digest = hex.EncodeToString(digest[:])
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		build.Modified = true
		return build
	}
	build.Version = moduleVersion(info.Main.Version)
	for _, setting := range info.Settings {
		applyBuildSetting(&build, setting)
	}
	return build
}

func executingExecutablePath() string {
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		return "/proc/self/exe"
	}
	path, _ := os.Executable()
	return path
}

func moduleVersion(value string) string {
	if value == "" || value == "(devel)" {
		return ""
	}
	return value
}

func applyBuildSetting(build *Build, setting debug.BuildSetting) {
	switch setting.Key {
	case "vcs.revision":
		build.Commit = setting.Value
	case "vcs.modified":
		build.Modified = setting.Value == "true"
	}
}
