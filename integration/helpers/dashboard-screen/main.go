package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/shaul/mesh/internal/terminal"
)

const maxCaptureBytes = 2 << 20

func capture() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("capture requires columns and rows")
	}
	columns, err := strconv.Atoi(os.Args[1])
	if err != nil {
		return fmt.Errorf("capture columns: %w", err)
	}
	rows, err := strconv.Atoi(os.Args[2])
	if err != nil {
		return fmt.Errorf("capture rows: %w", err)
	}
	return captureScreen(os.Stdin, os.Stdout, columns, rows)
}

func captureScreen(input io.Reader, output io.Writer, columns, rows int) error {
	data, err := io.ReadAll(io.LimitReader(input, maxCaptureBytes+1))
	if err != nil {
		return fmt.Errorf("capture input: %w", err)
	}
	if len(data) > maxCaptureBytes {
		return fmt.Errorf("capture input exceeds %d bytes", maxCaptureBytes)
	}
	screen := terminal.NewScreen(columns, rows)
	if _, err := screen.Write(data); err != nil {
		return fmt.Errorf("capture terminal: %w", err)
	}
	if err := json.NewEncoder(output).Encode(screen.Preview(columns, rows).Lines); err != nil {
		return fmt.Errorf("capture output: %w", err)
	}
	return nil
}
func main() {
	if err := capture(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
