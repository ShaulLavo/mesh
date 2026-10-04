package cli

import (
	"os"
	"slices"
	"strings"

	"github.com/shaul/mesh/internal/identity"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Command preserves identity operands across Cobra's option parsing.
type Command struct {
	*cobra.Command
}

func identityArgumentCommand(root *cobra.Command) *Command {
	restoreIdentityArguments(root)
	command := &Command{Command: root}
	command.SetArgs(os.Args[1:])
	return command
}

// SetArgs accepts the same unescaped arguments as the mesh executable.
func (c *Command) SetArgs(args []string) {
	if args == nil {
		args = os.Args[1:]
	}
	c.Command.SetArgs(protectIdentityArguments(c.Command, args))
}

// NUL cannot occur in an OS argument, so this marker cannot alias user input.
const identityArgumentMarker = "\x00"

func protectIdentityArguments(root *cobra.Command, args []string) []string {
	protected := slices.Clone(args)
	for i, arg := range protected {
		if leadingDashIdentity(arg) {
			protected[i] = identityArgumentMarker + arg
		}
	}
	command, _, err := root.Find(protected)
	if err != nil || command.DisableFlagParsing {
		return args
	}
	flags := command.Flags()
	flags.AddFlagSet(command.InheritedFlags())
	value := false
	for i, arg := range args {
		if arg == "--" && !value {
			copy(protected[i:], args[i:])
			break
		}
		if value {
			protected[i], value = arg, false
			continue
		}
		if !leadingDashIdentity(arg) {
			value = flagNeedsValue(flags, arg)
		}
	}
	return protected
}

func leadingDashIdentity(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	_, err := identity.IdentityKey(arg)
	return err == nil
}

func flagNeedsValue(flags *pflag.FlagSet, arg string) bool {
	if strings.HasPrefix(arg, "--") {
		name, _, attached := strings.Cut(arg[2:], "=")
		flag := flags.Lookup(name)
		return flag != nil && flag.NoOptDefVal == "" && !attached
	}
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	for i := 1; i < len(arg); i++ {
		flag := flags.ShorthandLookup(arg[i : i+1])
		if flag == nil || i+1 < len(arg) && arg[i+1] == '=' {
			return false
		}
		if flag.NoOptDefVal == "" {
			return i+1 == len(arg)
		}
	}
	return false
}

func restoreIdentityArguments(command *cobra.Command) {
	if validate := command.Args; validate != nil && !command.DisableFlagParsing {
		command.Args = func(cmd *cobra.Command, args []string) error {
			for i, arg := range args {
				args[i] = strings.TrimPrefix(arg, identityArgumentMarker)
			}
			return validate(cmd, args)
		}
	}
	for _, child := range command.Commands() {
		restoreIdentityArguments(child)
	}
}
