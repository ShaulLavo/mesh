package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func updateReaderCommand() *cobra.Command {
	return &cobra.Command{Use: "update-reader", Hidden: true, DisableFlagParsing: true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(command *cobra.Command, arguments []string) error {
			return RunUpdateReader(command.Context(), arguments, command.OutOrStdout(), command.ErrOrStderr())
		}}
}

// RunUpdateReader bypasses daemon and user-configuration initialization. The
// standard flag parser also accepts the original-reader single-dash contract.
func RunUpdateReader(ctx context.Context, arguments []string, output, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("update-reader", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	capabilities := flags.Bool("capabilities", false, "report the executing reader's compiled capability")
	input := flags.String("input", "", "bounded offline proof packet")
	cache := flags.String("trust-cache", "", "authenticated trust cache directory")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse update reader arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("update reader accepts no operands")
	}
	if *capabilities {
		if *input != "" || *cache != "" {
			return errors.New("update reader capabilities cannot be combined with a proof packet or trust cache")
		}
		return writeReaderResult(output, update.CurrentHostReaderCapabilities())
	}
	if *input == "" || !filepath.IsAbs(*cache) {
		return errors.New("update reader requires -input PACKET and an absolute -trust-cache DIR")
	}
	trustDirectory, err := os.OpenRoot(*cache)
	if err != nil {
		return fmt.Errorf("open authenticated reader trust cache: %w", err)
	}
	if err := trustDirectory.Close(); err != nil {
		return fmt.Errorf("close authenticated reader trust cache: %w", err)
	}
	packet, err := readReaderPacket(*input)
	if err != nil {
		return err
	}
	result, err := update.VerifyHostReaderInput(ctx, packet, *cache)
	if err != nil {
		return fmt.Errorf("verify update reader packet: %w", err)
	}
	return writeReaderResult(output, result)
}

func readReaderPacket(path string) (update.HostReaderInput, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return update.HostReaderInput{}, fmt.Errorf("open update reader packet directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return update.HostReaderInput{}, fmt.Errorf("open update reader packet: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return update.HostReaderInput{}, fmt.Errorf("inspect update reader packet: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > update.MaxHostReaderInputBytes {
		return update.HostReaderInput{}, errors.New("update reader packet must be a regular file of at most 8 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, update.MaxHostReaderInputBytes+1))
	if err != nil {
		return update.HostReaderInput{}, fmt.Errorf("read update reader packet: %w", err)
	}
	packet, err := update.DecodeHostReaderInput(data)
	if err != nil {
		return packet, fmt.Errorf("decode update reader packet: %w", err)
	}
	return packet, nil
}

func writeReaderResult(output io.Writer, result any) error {
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return fmt.Errorf("write update reader result: %w", err)
	}
	return nil
}
