// Command icns packs PNG renders into the macOS icon container embedded in Mesh.app.
//
// Usage: go run ./internal/macapp/cmd/icns -out mesh.icns 16=a.png 32=b.png ...
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// PNG-backed entries macOS reads; the @2x types reuse the next size up.
var entries = []struct {
	kind string
	size int
}{
	{"icp4", 16}, {"icp5", 32}, {"icp6", 64}, {"ic07", 128}, {"ic08", 256}, {"ic09", 512},
	{"ic10", 1024}, {"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512},
}

func main() {
	out := flag.String("out", "", "icns file to write")
	flag.Parse()
	if err := run(*out, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "icns:", err)
		os.Exit(1)
	}
}

func run(out string, args []string) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}
	pngs := map[int][]byte{}
	for _, arg := range args {
		size, file, ok := strings.Cut(arg, "=")
		if !ok {
			return fmt.Errorf("argument %q is not size=file.png", arg)
		}
		n, err := strconv.Atoi(size)
		if err != nil {
			return fmt.Errorf("argument %q: size: %w", arg, err)
		}
		data, err := os.ReadFile(file) //nolint:gosec // renders named on the command line by scripts/mac-icon.sh
		if err != nil {
			return fmt.Errorf("read %dpx render: %w", n, err)
		}
		pngs[n] = data
	}
	var body bytes.Buffer
	for _, entry := range entries {
		data, ok := pngs[entry.size]
		if !ok {
			return fmt.Errorf("no %dpx render for icns entry %s", entry.size, entry.kind)
		}
		if len(data) > math.MaxUint32-8 {
			return fmt.Errorf("%dpx render is too large for icns", entry.size)
		}
		body.WriteString(entry.kind)
		_ = binary.Write(&body, binary.BigEndian, uint32(len(data)+8)) //nolint:gosec // bounded above
		body.Write(data)
	}
	if body.Len() > math.MaxUint32-8 {
		return fmt.Errorf("icns body of %d bytes is too large", body.Len())
	}
	var file bytes.Buffer
	file.WriteString("icns")
	_ = binary.Write(&file, binary.BigEndian, uint32(body.Len()+8)) //nolint:gosec // bounded above
	file.Write(body.Bytes())
	if err := os.WriteFile(out, file.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}
