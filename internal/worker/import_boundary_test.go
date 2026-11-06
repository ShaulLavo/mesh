package worker

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const workerModulePrefix = "github.com/shaul/mesh/"

// This is an import-edge constraint, not runtime lifecycle independence.
func TestWorkerImportBoundary(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	graph, err := workerImportGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	worker := workerModulePrefix + "internal/worker"
	terminal := workerModulePrefix + "internal/terminal"
	if len(graph[worker]) == 0 {
		t.Fatal("import observer found no worker dependencies")
	}
	if _, ok := graph[terminal]; !ok {
		t.Fatal("import observer did not discover terminal package")
	}
	if path := workerForbiddenPath(graph, worker, workerForbiddenImport); len(path) != 0 {
		t.Fatalf("worker import boundary: %s", strings.Join(path, " -> "))
	}
	if path := workerForbiddenPath(graph, terminal, func(path string) bool {
		return strings.HasPrefix(path, workerModulePrefix)
	}); len(path) != 0 {
		t.Fatalf("terminal imports a Mesh package: %s", strings.Join(path, " -> "))
	}
	t.Logf("parsed %d own-module packages across all non-test platform files", len(graph))
}

func workerForbiddenImport(path string) bool {
	for _, name := range []string{
		"daemon", "transport", "identity", "storage", "tailnet", "sshd", "sshfs", "cli", "tui",
		"apps", "webauth", "dnsname", "fleetshare", "update", "updateinstall", "updatebootstrap", "wakeclient", "usagefeed",
	} {
		forbidden := workerModulePrefix + "internal/" + name
		if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
			return true
		}
	}
	return false
}

func workerImportGraph(root string) (map[string][]string, error) {
	graph := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk import source %s: %w", path, err)
		}
		if entry.IsDir() {
			return workerImportDirectory(root, path, entry.Name())
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		return workerFileImports(root, path, graph)
	})
	if err != nil {
		return nil, fmt.Errorf("read own-module import graph: %w", err)
	}
	return graph, nil
}

func workerImportDirectory(root, path, name string) error {
	if path == root {
		return nil
	}
	if name == ".git" || name == "vendor" {
		return filepath.SkipDir
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	if err == nil {
		return filepath.SkipDir
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("check nested module in %s: %w", path, err)
	}
	return nil
}

func workerFileImports(root, path string, graph map[string][]string) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return fmt.Errorf("parse imports in %s: %w", path, err)
	}
	relative, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("locate import package for %s: %w", path, err)
	}
	pkg := workerModulePrefix + filepath.ToSlash(relative)
	imports := graph[pkg]
	for _, imported := range file.Imports {
		dependency, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return fmt.Errorf("decode import in %s: %w", path, err)
		}
		if strings.HasPrefix(dependency, workerModulePrefix) {
			imports = append(imports, dependency)
		}
	}
	slices.Sort(imports)
	graph[pkg] = slices.Compact(imports)
	return nil
}

func workerForbiddenPath(graph map[string][]string, root string, forbidden func(string) bool) []string {
	return workerImportPathFrom(graph, []string{root}, map[string]bool{root: true}, forbidden)
}

func workerImportPathFrom(graph map[string][]string, path []string, seen map[string]bool, forbidden func(string) bool) []string {
	next := slices.Clone(graph[path[len(path)-1]])
	slices.Sort(next)
	for _, dependency := range next {
		if seen[dependency] {
			continue
		}
		seen[dependency] = true
		branch := append(slices.Clone(path), dependency)
		if forbidden(dependency) {
			return branch
		}
		if found := workerImportPathFrom(graph, branch, seen, forbidden); len(found) != 0 {
			return found
		}
	}
	return nil
}

func TestWorkerImportGraphControls(t *testing.T) {
	forbidden := func(path string) bool { return path == "daemon" }
	for _, tc := range []struct {
		name  string
		graph map[string][]string
		want  []string
	}{
		{name: "legal", graph: map[string][]string{"worker": {"protocol"}, "protocol": {"serve"}}},
		{name: "cycle terminates", graph: map[string][]string{"worker": {"protocol"}, "protocol": {"worker"}}},
		{name: "forbidden two hops", graph: map[string][]string{"worker": {"protocol"}, "protocol": {"daemon"}}, want: []string{"worker", "protocol", "daemon"}},
		{name: "deterministic path through cycle", graph: map[string][]string{"worker": {"z", "a"}, "a": {"worker", "daemon"}, "z": {"daemon"}}, want: []string{"worker", "a", "daemon"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workerForbiddenPath(tc.graph, "worker", forbidden)
			workerRequireImportPath(t, got, tc.want)
			t.Logf("dependency path = %v", got)
		})
	}
}

func workerRequireImportPath(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("forbidden path = %v, want %v", got, want)
	}
}

func TestWorkerImportParserIncludesOtherPlatforms(t *testing.T) {
	root := t.TempDir()
	for name, source := range map[string]string{
		"internal/worker/worker.go":         "package worker\nimport _ \"github.com/shaul/mesh/internal/protocol\"\n",
		"internal/protocol/types.go":        "package protocol\n",
		"internal/protocol/only_windows.go": "//go:build windows\n\npackage protocol\nimport _ \"github.com/shaul/mesh/internal/daemon\"\n",
		"internal/worker/ignored_test.go":   "package worker\nimport _ \"github.com/shaul/mesh/internal/storage\"\n",
		"nested/go.mod":                     "module fixture.invalid/nested\n",
		"nested/ignored.go":                 "package nested\nimport _ \"github.com/shaul/mesh/internal/identity\"\n",
	} {
		workerWriteImportFixture(t, root, name, source)
	}
	graph, err := workerImportGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph) != 2 {
		t.Fatalf("parsed %d packages, want two own-module non-test packages: %v", len(graph), graph)
	}
	workerRequireImportPath(t, graph[workerModulePrefix+"internal/worker"], []string{workerModulePrefix + "internal/protocol"})
	path := workerForbiddenPath(graph, workerModulePrefix+"internal/worker", workerForbiddenImport)
	workerRequireImportPath(t, path, []string{
		workerModulePrefix + "internal/worker", workerModulePrefix + "internal/protocol", workerModulePrefix + "internal/daemon",
	})
	t.Logf("other-platform forbidden path: %s; test files and nested module excluded", strings.Join(path, " -> "))
}

func workerWriteImportFixture(t *testing.T, root, name, source string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}
