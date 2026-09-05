package agentresume

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type option struct {
	name  string
	value bool
}

func providerOption(provider Provider, name string) (option, bool) {
	if provider == Codex {
		return codexOption(name)
	}
	return claudeOption(name)
}

func codexOption(name string) (option, bool) {
	switch name {
	case "-m", "--model":
		return option{"--model", true}, true
	case "-p", "--profile":
		return option{"--profile", true}, true
	case "-s", "--sandbox":
		return option{"--sandbox", true}, true
	case "-a", "--ask-for-approval":
		return option{"--ask-for-approval", true}, true
	case "--add-dir":
		return option{name, true}, true
	case "--search", "--no-alt-screen", "--approve-for-me", "--dangerously-bypass-approvals-and-sandbox":
		return option{name, false}, true
	default:
		return option{}, false
	}
}

func claudeOption(name string) (option, bool) {
	switch name {
	case "--model", "--effort", "--permission-mode":
		return option{name, true}, true
	case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--verbose":
		return option{name, false}, true
	default:
		return option{}, false
	}
}

func validateOptions(provider Provider, options []string) error {
	if len(options) > MaxOptions {
		return fmt.Errorf("agent recovery: too many launch options")
	}
	bytes := 0
	for _, value := range options {
		bytes += len(value)
	}
	if bytes > MaxOptionBytes {
		return fmt.Errorf("agent recovery: saved launch options exceed %d bytes", MaxOptionBytes)
	}
	for index := 0; index < len(options); index++ {
		opt, ok := providerOption(provider, options[index])
		if !ok || opt.name != options[index] {
			return fmt.Errorf("agent recovery: unsupported saved launch option")
		}
		if !opt.value {
			continue
		}
		index++
		if index >= len(options) || !optionValue(options[index]) {
			return fmt.Errorf("agent recovery: missing or invalid option value")
		}
	}
	return nil
}

func optionValue(value string) bool { return field(value) && !strings.HasPrefix(value, "-") }

// ParseLaunch reduces supported interactive arguments to a resume recipe. The
// caller still runs the original command if this returns an unsupported option.
func ParseLaunch(provider Provider, executable, version, cwd string, env, argv []string) (Launch, error) {
	launch := Launch{Provider: provider, Executable: executable, ProviderVersion: version, Directory: cwd}
	if err := ValidateProvider(provider); err != nil {
		return launch, err
	}
	if err := validateLaunchEnvironment(provider, env); err != nil {
		return launch, err
	}
	root, err := dataRoot(provider, env)
	if err != nil {
		return launch, err
	}
	launch.DataRoot = root
	if err := parseArguments(&launch, argv); err != nil {
		return launch, err
	}
	return launch, ValidateLaunch(launch)
}

func validateLaunchEnvironment(provider Provider, env []string) error {
	if provider != Claude {
		return nil
	}
	for _, key := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_AWS_BASE_URL",
	} {
		value := envValue(env, key)
		if strings.HasPrefix(key, "CLAUDE_CODE_USE_") && (value == "0" || strings.EqualFold(value, "false")) {
			continue
		}
		if value != "" {
			return fmt.Errorf("agent recovery: %s selects provider routing that is not saved for recovery", key)
		}
	}
	return nil
}

func parseArguments(launch *Launch, argv []string) error {
	if len(argv) > 0 && noninteractiveCommand(launch.Provider, argv[0]) {
		return fmt.Errorf("agent recovery: provider subcommands use their native lifecycle")
	}
	if launch.Provider == Codex && len(argv) > 0 && (argv[0] == "resume" || argv[0] == "fork") {
		argv = argv[1:]
	}
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if arg == "--" {
			return nil
		}
		if !strings.HasPrefix(arg, "-") {
			if noninteractiveCommand(launch.Provider, arg) {
				return fmt.Errorf("agent recovery: provider subcommands use their native lifecycle")
			}
			continue
		}
		consumed, err := parseOption(launch, arg, argv[index+1:])
		if err != nil {
			return err
		}
		index += consumed
	}
	return nil
}

func noninteractiveCommand(provider Provider, value string) bool {
	if provider == Codex {
		return slices.Contains([]string{"exec", "e", "review", "login", "logout", "mcp", "mcp-server", "app-server", "completion", "sandbox", "debug", "apply", "cloud", "features", "help"}, value)
	}
	return slices.Contains([]string{"agents", "attach", "auth", "auto-mode", "doctor", "gateway", "import", "install", "logs", "mcp", "plugin", "plugins", "project", "respawn", "rm", "setup-token", "stop", "kill", "ultrareview", "update", "upgrade"}, value)
}

func parseOption(launch *Launch, arg string, rest []string) (int, error) {
	name, inline, hasInline := strings.Cut(arg, "=")
	if initialFlag(launch.Provider, name) && !hasInline {
		return 0, nil
	}
	if launch.Provider == Codex && (name == "-C" || name == "--cd") {
		value, consumed, err := takeValue(inline, hasInline, rest)
		if err != nil {
			return 0, err
		}
		launch.Directory = resolveDirectory(launch.Directory, value)
		return consumed, nil
	}
	if launch.Provider == Claude && (name == "--resume" || name == "-r" || name == "--session-id") {
		_, consumed, err := takeValue(inline, hasInline, rest)
		return consumed, err
	}
	opt, ok := providerOption(launch.Provider, name)
	if !ok || (!opt.value && hasInline) {
		return 0, fmt.Errorf("agent recovery: launch option %s is not supported for automatic recovery", name)
	}
	launch.Options = append(launch.Options, opt.name)
	if !opt.value {
		return 0, nil
	}
	value, consumed, err := takeValue(inline, hasInline, rest)
	if name == "--add-dir" && err == nil && !filepath.IsAbs(value) {
		return 0, fmt.Errorf("agent recovery: use an absolute --add-dir path to preserve its meaning on resume")
	}
	launch.Options = append(launch.Options, value)
	return consumed, err
}

func initialFlag(provider Provider, name string) bool {
	if provider == Codex {
		return name == "--last" || name == "--all"
	}
	return name == "--continue" || name == "-c" || name == "--fork-session"
}

func takeValue(inline string, hasInline bool, rest []string) (string, int, error) {
	if hasInline && optionValue(inline) {
		return inline, 0, nil
	}
	if !hasInline && len(rest) > 0 && optionValue(rest[0]) {
		return rest[0], 1, nil
	}
	return "", 0, fmt.Errorf("agent recovery: missing or invalid launch option value")
}

func resolveDirectory(base, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(base, value)
}

func dataRoot(provider Provider, env []string) (string, error) {
	key, fallback := "CODEX_HOME", ".codex"
	if provider == Claude {
		key, fallback = "CLAUDE_CONFIG_DIR", ".claude"
	}
	root := envValue(env, key)
	if root == "" {
		root = filepath.Join(envValue(env, "HOME"), fallback)
	}
	if !absoluteField(root) {
		return "", fmt.Errorf("agent recovery: %s must resolve to an absolute data root", key)
	}
	return filepath.Clean(root), nil
}

func envValue(env []string, name string) string {
	for index := len(env) - 1; index >= 0; index-- {
		if value, ok := strings.CutPrefix(env[index], name+"="); ok {
			return value
		}
	}
	return ""
}

func ResumeCommand(recipe Recipe) ([]string, error) {
	if err := validateResume(recipe); err != nil {
		return nil, err
	}
	argv := []string{recipe.Executable}
	if recipe.Provider == Codex {
		argv = append(argv, "resume")
		argv = append(argv, recipe.Options...)
		return append(argv, "--cd", recipe.Directory, "--", recipe.ConversationID), nil
	}
	argv = append(argv, recipe.Options...)
	return append(argv, "--resume="+recipe.ConversationID), nil
}

func ResumeEnv(recipe Recipe, inherited []string) ([]string, error) {
	if err := validateResume(recipe); err != nil {
		return nil, err
	}
	key := "CODEX_HOME"
	if recipe.Provider == Claude {
		key = "CLAUDE_CONFIG_DIR"
	}
	env := slices.DeleteFunc(slices.Clone(inherited), func(entry string) bool {
		return strings.HasPrefix(entry, key+"=") || strings.HasPrefix(entry, "MESH_AGENT_")
	})
	return append(env, key+"="+recipe.DataRoot), nil
}

func CheckAvailable(recipe Recipe) error {
	if err := validateResume(recipe); err != nil {
		return err
	}
	for _, directory := range []string{recipe.Directory, recipe.DataRoot} {
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("agent recovery: saved directory %s is unavailable", directory)
		}
	}
	info, err := os.Stat(recipe.Executable)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("agent recovery: saved provider executable %s is unavailable", recipe.Executable)
	}
	return nil
}

func validateResume(recipe Recipe) error {
	if recipe.Version != Version || !field(recipe.ConversationID) {
		return fmt.Errorf("agent recovery: invalid native resume identity")
	}
	return ValidateLaunch(recipe.Launch)
}

// Compatibility reports native evidence separately from schema validation.
func Compatibility(launch Launch) string {
	if err := ValidateLaunch(launch); err != nil {
		return err.Error()
	}
	if launch.Provider == Claude && launch.ProviderVersion == "2.1.261 (Claude Code)" {
		return ""
	}
	if launch.Provider == Codex && launch.ProviderVersion == "codex-cli 0.153.4" {
		return ""
	}
	return "native invocation association and exact resume have not been verified for this provider version; use explicit binding"
}
