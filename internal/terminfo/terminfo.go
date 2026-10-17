// Package terminfo carries terminal definitions to the account running a remote shell.
package terminfo

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const MaximumBytes = 64 << 10

// Capture resolves the caller's terminal database, including terminal-provided
// TERMINFO paths. Source text lets each destination's ncurses compiler choose
// its own binary format, including macOS' older system ncurses.
func Capture(ctx context.Context, name string) []byte {
	if !validName(name) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	source, err := exec.CommandContext(ctx, "infocmp", "-x", name).Output() //nolint:gosec // name is validated and passed as an argument to a fixed ncurses tool
	if err != nil || len(source) > MaximumBytes {
		return nil
	}
	return source
}

// Install publishes exactly the requested entry in the host ncurses layout.
// Renaming a complete file lets concurrent creates safely share this database.
func Install(ctx context.Context, name string, source []byte) error {
	if len(source) == 0 {
		return nil
	}
	if !validName(name) || len(source) > MaximumBytes {
		return fmt.Errorf("invalid terminal definition for %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "infocmp", "-x", name).Run() == nil { //nolint:gosec // name is validated and passed as an argument to a fixed ncurses tool
		return nil
	}
	data, err := compile(ctx, name, source)
	if err != nil {
		return err
	}
	return publish(name, data)
}

func compile(ctx context.Context, name string, source []byte) ([]byte, error) {
	directory, err := os.MkdirTemp("", "mesh-terminfo-*")
	if err != nil {
		return nil, fmt.Errorf("prepare terminal definition %s: %w", name, err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	compiler := exec.CommandContext(ctx, "tic", "-x", "-o", directory, "-") //nolint:gosec // fixed compiler writes to a task-owned scratch directory
	compiler.Stdin = bytes.NewReader(source)
	if err := compiler.Run(); err != nil {
		return nil, fmt.Errorf("compile terminal definition %s: %w; install ncurses terminal tools on this host", name, err)
	}
	return compiledEntry(directory, name)
}

func publish(name string, data []byte) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find terminal database home: %w", err)
	}
	prefix := name[:1]
	if runtime.GOOS == "darwin" {
		prefix = strconv.FormatUint(uint64(name[0]), 16)
	}
	directory := filepath.Join(home, ".terminfo", prefix)
	target := filepath.Join(directory, name)
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) { //nolint:gosec // target is a validated terminal name under this account home
		return nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create terminal database: %w", err)
	}
	file, err := os.CreateTemp(directory, ".mesh-*")
	if err != nil {
		return fmt.Errorf("write terminal definition %s: %w", name, err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write terminal definition %s: %w", name, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close terminal definition %s: %w", name, closeErr)
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return fmt.Errorf("publish terminal definition %s: %w", name, err)
	}
	return nil
}

func compiledEntry(directory, name string) ([]byte, error) {
	for _, prefix := range []string{name[:1], strconv.FormatUint(uint64(name[0]), 16)} {
		data, err := os.ReadFile(filepath.Join(directory, prefix, name)) //nolint:gosec // requested name is validated and compiler output is in task-owned scratch
		if err == nil && validate(name, data) == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("compiler did not produce terminal definition %s", name)
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 128 || name[0] == '-' {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '+' || character == '.' {
			continue
		}
		return false
	}
	return name != "." && name != ".."
}

func validate(name string, data []byte) error {
	if !validName(name) || len(data) < 12 || len(data) > MaximumBytes {
		return fmt.Errorf("invalid terminal definition for %q", name)
	}
	var order binary.ByteOrder = binary.LittleEndian
	magic := order.Uint16(data[:2])
	if magic != 0x011a && magic != 0x021e {
		order = binary.BigEndian
		magic = order.Uint16(data[:2])
	}
	if magic != 0x011a && magic != 0x021e {
		return fmt.Errorf("invalid terminal definition header for %s", name)
	}
	size := int(order.Uint16(data[2:4]))
	if size < 2 || 12+size > len(data) || data[11+size] != 0 {
		return fmt.Errorf("invalid terminal definition names for %s", name)
	}
	names := strings.Split(string(data[12:11+size]), "|")
	for _, alias := range names {
		if alias == name {
			return nil
		}
	}
	return fmt.Errorf("terminal definition does not name %s", name)
}
