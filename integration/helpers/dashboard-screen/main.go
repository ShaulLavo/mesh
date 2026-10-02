package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/shaul/mesh/internal/terminal"
)

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
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("capture input: %w", err)
	}
	screen := terminal.NewScreen(columns, rows)
	if _, err := screen.Write(data); err != nil {
		return fmt.Errorf("capture terminal: %w", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(screen.Preview(columns, rows).Lines); err != nil {
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
