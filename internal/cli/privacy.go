package cli

import (
	"github.com/shaul/mesh/internal/privacy"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type privacyError struct{ cause error }

func (e privacyError) Error() string {
	return "Privacy mode: error details hidden; use --privacy=false to inspect"
}
func (e privacyError) Unwrap() error { return e.cause }

func protectPrivacyErrors(command *cobra.Command, enabled *bool) {
	protect := func(err error) error {
		if err == nil || !*enabled {
			return err
		}
		return privacyError{cause: err}
	}
	if run := command.RunE; run != nil {
		command.RunE = func(cmd *cobra.Command, args []string) error { return protect(run(cmd, args)) }
	}
	if args := command.Args; args != nil {
		command.Args = func(cmd *cobra.Command, values []string) error { return protect(args(cmd, values)) }
	}
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return protect(err) })
	for _, child := range command.Commands() {
		protectPrivacyErrors(child, enabled)
	}
}

// PrivacyErrorForArguments also covers parser failures before Cobra reaches a
// later privacy flag. Program arguments after -- belong to the child instead.
func PrivacyErrorForArguments(err error, arguments []string) error {
	if err == nil {
		return nil
	}
	enabled := privacy.EnabledFromEnv()
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--privacy" {
			enabled = true
		}
		if value, found := strings.CutPrefix(argument, "--privacy="); found {
			if decision, parseErr := strconv.ParseBool(value); parseErr == nil {
				enabled = decision
			}
		}
	}
	if enabled {
		return privacyError{cause: err}
	}
	return err
}
