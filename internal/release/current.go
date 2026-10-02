package release

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
	"sync"
)

// Version is set by the release linker. Development builds leave it empty.
var Version string

var buildMetadata = readBuildMetadata()
var executingBuild = newExecutingBuild(executingExecutablePath())

// Metadata reports build settings without reading or hashing the executable.
// Its empty digest must not be used for update or compatibility decisions.
func Metadata() Build {
	build := buildMetadata
	if Version != "" {
		build.Version = Version
	}
	return build
}

// Current lazily hashes the executable image once per process for build identity.
func Current() Build {
	build := executingBuild()
	if Version != "" {
		build.Version = Version
	}
	return build
}

func readBuildMetadata() Build {
	build := Build{
		Platform:       CurrentPlatform(),
		StateVersion:   CurrentStateVersion,
		WorkerProtocol: CurrentWorkerProtocol,
		UpdateProtocol: CurrentUpdateProtocol,
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		build.Modified = true
		return build
	}
	build.Version = moduleVersion(info.Main.Version)
	for _, setting := range info.Settings {
		applyBuildSetting(&build, setting)
	}
	return build
}

// Pin the image before an update replaces its pathname, including on macOS
// where /proc/self/exe is unavailable. Opening it does not read its contents.
func newExecutingBuild(path string) func() Build {
	file, err := os.Open(path) //nolint:gosec // path names this process executable
	return sync.OnceValue(func() Build { return readExecutingBuild(file, err) })
}

func readExecutingBuild(file *os.File, err error) Build {
	build := buildMetadata
	if err != nil {
		build.Modified = true
		return build
	}
	defer file.Close() //nolint:errcheck // read-only
	// Stream the image so identity checks do not retain binary-sized allocations.
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		build.Modified = true
		return build
	}
	build.Digest = hex.EncodeToString(hash.Sum(nil))
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
