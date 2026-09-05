package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/worker"
)

func (a *application) shellInitCommand() *cobra.Command {
	var agents bool
	command := &cobra.Command{
		Use: "shell-init bash|zsh", Short: "Print opt-in directory and history recovery hooks",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate Mesh shell helper: %w", err)
			}
			snippet, err := shellInit(args[0], executable)
			if err != nil {
				return err
			}
			if agents {
				snippet += agentShellInit(args[0], executable)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), snippet)
			return err
		},
	}
	command.Flags().BoolVar(&agents, "agents", false, "also wrap native codex and claude commands for conversation recovery")
	return command
}

func agentShellInit(shell, executable string) string {
	helper := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	fragment := bashAgentFunctions
	if shell == "zsh" {
		fragment = zshAgentFunctions
	}
	return strings.ReplaceAll(fragment, "@MESH@", helper)
}

const bashAgentFunctions = `if [[ $- == *i* ]]; then
  if ! declare -F codex >/dev/null && ! alias codex >/dev/null 2>&1; then
    function codex { @MESH@ agent codex -- "$@"; }
  fi
  if ! declare -F claude >/dev/null && ! alias claude >/dev/null 2>&1; then
    function claude { @MESH@ agent claude -- "$@"; }
  fi
fi
`

const zshAgentFunctions = `if [[ -o interactive ]]; then
  if (( ! $+functions[codex] && ! $+aliases[codex] )); then
    function codex { @MESH@ agent codex -- "$@"; }
  fi
  if (( ! $+functions[claude] && ! $+aliases[claude] )); then
    function claude { @MESH@ agent claude -- "$@"; }
  fi
fi
`

func shellInit(shell, executable string) (string, error) {
	helper := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	switch shell {
	case "bash":
		return strings.ReplaceAll(bashRecoveryHook, "@MESH@", helper), nil
	case "zsh":
		return strings.ReplaceAll(zshRecoveryHook, "@MESH@", helper), nil
	default:
		return "", fmt.Errorf("shell-init supports bash and zsh, got %q", shell)
	}
}

func (a *application) shellUpdateCommand() *cobra.Command {
	return &cobra.Command{
		Use: "shell-update SHELL", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			location, ok := worker.ContainingSessionWorker()
			if !ok {
				return statusError{code: 1}
			}
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("read shell directory: %w", err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 750*time.Millisecond)
			defer cancel()
			response, err := daemonControlRequest(ctx, paths.Socket(location.Dir), protocol.Control{
				Type: protocol.TypeShellUpdate, SessionID: location.SessionID,
				ShellPID: os.Getppid(), ShellDirectory: cwd, ShellExecutable: args[0],
			})
			if err != nil {
				return err
			}
			if response.Type != protocol.TypeShellUpdated {
				return fmt.Errorf("save shell directory: %s", response.Message)
			}
			return nil
		},
	}
}

const bashRecoveryHook = `if [[ $- == *i* && ${__mesh_recovery_pid-} != "$$" ]]; then
  __mesh_recovery_pid=$$
  __mesh_recovery_prompt() {
    local mesh_prompt_status=$?
    if @MESH@ shell-update "$BASH" >/dev/null 2>&1; then
      if [[ -o history && -n ${HISTFILE-} && ${HISTSIZE:-0} -gt 0 ]]; then
        builtin history -a >/dev/null 2>&1 || :
      fi
    fi
    return "$mesh_prompt_status"
  }
  if [[ $(declare -p PROMPT_COMMAND 2>/dev/null) == "declare -a "* ]]; then
    PROMPT_COMMAND=(__mesh_recovery_prompt "${PROMPT_COMMAND[@]}")
  else
    PROMPT_COMMAND="__mesh_recovery_prompt${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
  fi
fi
`

const zshRecoveryHook = `if [[ -o interactive && ${__mesh_recovery_pid-} != "$$" ]]; then
  typeset -g __mesh_recovery_pid=$$
  __mesh_recovery_prompt() {
    local mesh_prompt_status=$?
    if @MESH@ shell-update "${commands[zsh]:-zsh}" >/dev/null 2>&1; then
      if [[ -n ${HISTFILE-} && ${SAVEHIST:-0} -gt 0 ]]; then
        builtin fc -AI "$HISTFILE" >/dev/null 2>&1 || :
      fi
    fi
    return "$mesh_prompt_status"
  }
  typeset -ga precmd_functions
  precmd_functions=(__mesh_recovery_prompt ${precmd_functions:#__mesh_recovery_prompt})
fi
`
