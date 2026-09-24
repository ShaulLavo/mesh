package release

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"io"
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
	file, err := os.Open(executingExecutablePath())
	if err != nil {
		build.Modified = true
		return build
	}
	defer file.Close() //nolint:errcheck // read-only
	// Stream the image: every mesh process runs this at init, and reading the
	// whole binary into the heap left each long-lived worker tens of MB larger.
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		build.Modified = true
		return build
	}
	build.Digest = hex.EncodeToString(hash.Sum(nil))
	info, err := buildinfo.Read(file)
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
