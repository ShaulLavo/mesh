package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	golangciGate   = "golangci"
	deadcodeGate   = "deadcode"
	shellcheckGate = "shellcheck"
	ruffGate       = "ruff"
)

var gateNames = []string{golangciGate, deadcodeGate, shellcheckGate, ruffGate}
var goLocation = regexp.MustCompile(`(\.go):\d+(?:-\d+)?`)
var cloneRange = regexp.MustCompile(`^\d+-\d+ lines are duplicate of `)
var constantLiteral = regexp.MustCompile("(?s)^string (`.*`) has [0-9]+ occurrences.*$")
var cognitiveFunction = regexp.MustCompile("^cognitive complexity [0-9]+ of func (`.*`) is high \\(> [0-9]+\\)$")
var complexityScore = regexp.MustCompile(`\(complexity: \d+\)$`)
var diagnosticLine = regexp.MustCompile(`\bline \d+\b`)
var closureOrdinal = regexp.MustCompile(`\$[0-9]+`)

func normalizeText(rule, text string) string {
	if rule == "goconst" {
		return constantLiteral.ReplaceAllString(text, "string $1")
	}
	if rule == "nestif" {
		return complexityScore.ReplaceAllString(text, "(complexity: <score>)")
	}
	text = goLocation.ReplaceAllString(text, "${1}:<location>")
	switch rule {
	case "gocognit":
		return cognitiveFunction.ReplaceAllString(text, "func $1")
	case "nilerr":
		return diagnosticLine.ReplaceAllString(text, "line <location>")
	case "contextcheck":
		return closureOrdinal.ReplaceAllLiteralString(text, "$N")
	case "dupl":
		return cloneRange.ReplaceAllString(text, "<range> lines are duplicate of ")
	default:
		return text
	}
}

// Function context survives formatting and movement; repeated findings remain counted.
type findingKey struct {
	Gate     string `json:"gate"`
	File     string `json:"file"`
	Rule     string `json:"rule"`
	Text     string `json:"text"`
	Function string `json:"function"`
	Source   string `json:"source,omitempty"`
}

type entry struct {
	findingKey
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

type baseline struct {
	Version int     `json:"version"`
	Entries []entry `json:"entries"`
}

type position struct {
	Filename string
	Line     int
}

type collector struct {
	root      string
	fs        *os.Root
	lines     map[string][]string
	functions map[string][]functionRange
	counts    map[findingKey]int
}

func readJSON(path string, value any) error {
	directory, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("report directory %s: %w", path, err)
	}
	defer func() { _ = directory.Close() }()
	data, err := directory.ReadFile(filepath.Base(path))
	if err != nil {
		return fmt.Errorf("report %s: %w", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("report %s: %w", path, err)
	}
	return nil
}

func (c *collector) add(gate, file, rule, text string, line int) error {
	key, err := c.key(gate, file, rule, text, line)
	if err != nil {
		return err
	}
	if key.File != "" {
		c.counts[key]++
	}
	return nil
}

func (c *collector) reportFile(file string) (string, error) {
	if filepath.IsAbs(file) {
		name, err := filepath.Rel(c.root, file)
		if err != nil {
			return "", fmt.Errorf("report path %s: %w", file, err)
		}
		file = name
	}
	file = filepath.ToSlash(filepath.Clean(file))
	if file == ".." || strings.HasPrefix(file, "../") || file == "." {
		return "", fmt.Errorf("invalid report path: %s", file)
	}
	if file == "third_party" || strings.HasPrefix(file, "third_party/") {
		return "", nil
	}
	return file, nil
}

func (c *collector) key(gate, file, rule, text string, line int) (findingKey, error) {
	file, err := c.reportFile(file)
	if err != nil {
		return findingKey{}, err
	}
	if file == "" {
		return findingKey{}, nil
	}
	if rule == "" || text == "" {
		return findingKey{}, fmt.Errorf("invalid report finding: %s [%s] %s", file, rule, text)
	}
	key := findingKey{Gate: gate, File: file, Rule: rule, Text: normalizeText(rule, text)}
	if line == 0 {
		return key, nil
	}
	source, err := c.source(file, line)
	if err != nil {
		return findingKey{}, err
	}
	// Non-Go exceptions stay attached to the statement they originally deferred.
	if gate != golangciGate {
		key.Source = source
		return key, nil
	}
	// Goconst reports a file-scoped literal at an arbitrary representative occurrence.
	if rule == "goconst" {
		return key, nil
	}
	key.Function, err = c.function(file, line)
	return key, err
}

type functionRange struct {
	first, last int
	name        string
}

func receiverName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverName(value.X)
	case *ast.IndexExpr:
		return receiverName(value.X)
	case *ast.IndexListExpr:
		return receiverName(value.X)
	default:
		return ""
	}
}

func functionName(function *ast.FuncDecl) string {
	if function.Recv == nil {
		return function.Name.Name
	}
	return receiverName(function.Recv.List[0].Type) + "." + function.Name.Name
}

func (c *collector) parseFunctions(file string) ([]functionRange, error) {
	set := token.NewFileSet()
	tree, err := parser.ParseFile(set, file, strings.Join(c.lines[file], "\n"), parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("source functions %s: %w", file, err)
	}
	functions := []functionRange{}
	for _, declaration := range tree.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		functions = append(functions, functionRange{set.Position(function.Pos()).Line, set.Position(function.End()).Line, functionName(function)})
	}
	return functions, nil
}

func (c *collector) function(file string, line int) (string, error) {
	functions, ok := c.functions[file]
	if !ok {
		var err error
		functions, err = c.parseFunctions(file)
		if err != nil {
			return "", err
		}
		if c.functions == nil {
			c.functions = make(map[string][]functionRange)
		}
		c.functions[file] = functions
	}
	for _, function := range functions {
		if line >= function.first && line <= function.last {
			return function.name, nil
		}
	}
	return "", nil
}

func (c *collector) source(file string, line int) (string, error) {
	lines, ok := c.lines[file]
	if !ok {
		data, err := c.fs.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("source %s: %w", file, err)
		}
		lines = strings.Split(string(data), "\n")
		c.lines[file] = lines
	}
	if line < 1 || line > len(lines) {
		return "", fmt.Errorf("invalid report location: %s:%d", file, line)
	}
	return strings.TrimSpace(lines[line-1]), nil
}

func (c *collector) golangci(path string) error {
	var report struct {
		Issues json.RawMessage
	}
	if err := readJSON(path, &report); err != nil {
		return err
	}
	if len(report.Issues) == 0 {
		return errors.New("golangci report is missing Issues")
	}
	var issues []struct {
		FromLinter string
		Text       string
		Pos        position
	}
	if err := json.Unmarshal(report.Issues, &issues); err != nil {
		return fmt.Errorf("golangci issues: %w", err)
	}
	for _, issue := range issues {
		if issue.Pos.Line < 1 {
			return errors.New("golangci report is missing a source location")
		}
		if err := c.add(golangciGate, issue.Pos.Filename, issue.FromLinter, issue.Text, issue.Pos.Line); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) deadcode(path string) error {
	var packages []struct {
		Funcs []struct {
			Name     string
			Position struct{ File string }
		}
	}
	if err := readJSON(path, &packages); err != nil {
		return err
	}
	for _, pkg := range packages {
		for _, function := range pkg.Funcs {
			if err := c.add(deadcodeGate, function.Position.File, "unreachable", function.Name, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *collector) shellcheck(path string) error {
	var issues []struct {
		File    string
		Code    int
		Message string
		Line    int
	}
	if err := readJSON(path, &issues); err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.Line < 1 || issue.Code < 1 {
			return errors.New("shellcheck report is missing a source location or code")
		}
		if err := c.add(shellcheckGate, issue.File, fmt.Sprintf("SC%d", issue.Code), issue.Message, issue.Line); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) ruff(path string) error {
	var issues []struct {
		Filename string
		Code     string
		Message  string
		Location struct{ Row int }
	}
	if err := readJSON(path, &issues); err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.Location.Row < 1 {
			return errors.New("ruff report is missing a source location")
		}
		if err := c.add(ruffGate, issue.Filename, issue.Code, issue.Message, issue.Location.Row); err != nil {
			return err
		}
	}
	return nil
}

func collect(reports, root string) (map[findingKey]int, error) {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("source directory %s: %w", root, err)
	}
	defer func() { _ = directory.Close() }()
	c := collector{root: root, fs: directory, lines: make(map[string][]string), counts: make(map[findingKey]int)}
	for _, gate := range []struct {
		name string
		read func(string) error
	}{
		{golangciGate, c.golangci}, {deadcodeGate, c.deadcode}, {shellcheckGate, c.shellcheck}, {ruffGate, c.ruff},
	} {
		if err := gate.read(filepath.Join(reports, gate.name+".json")); err != nil {
			return nil, fmt.Errorf("%s report: %w", gate.name, err)
		}
	}
	return c.counts, nil
}

func readBaseline(path string) (map[findingKey]entry, error) {
	var data baseline
	if err := readJSON(path, &data); err != nil {
		return nil, err
	}
	if data.Version != 2 {
		return nil, errors.New("unsupported baseline version")
	}
	entries := make(map[findingKey]entry)
	for _, item := range data.Entries {
		if !slices.Contains(gateNames, item.Gate) || item.File == "" || item.Rule == "" || item.Text == "" || item.Count < 1 || strings.TrimSpace(item.Reason) == "" {
			return nil, fmt.Errorf("invalid baseline entry or missing reason: %s [%s]", item.File, item.Rule)
		}
		if item.Text != normalizeText(item.Rule, item.Text) {
			return nil, fmt.Errorf("unnormalized baseline key: %s [%s]", item.File, item.Rule)
		}
		if _, exists := entries[item.findingKey]; exists {
			return nil, fmt.Errorf("duplicate baseline entry: %s [%s]", item.File, item.Rule)
		}
		entries[item.findingKey] = item
	}
	return entries, nil
}

func keyText(key findingKey) string {
	return strings.Join([]string{key.Gate, key.File, key.Rule, key.Text, key.Function, key.Source}, "\x00")
}

func sortedKeys[V any](values map[findingKey]V) []findingKey {
	keys := make([]findingKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b findingKey) int { return strings.Compare(keyText(a), keyText(b)) })
	return keys
}

func writeBaseline(path string, entries map[findingKey]entry) error {
	var buffer bytes.Buffer
	buffer.WriteString("{\n  \"version\": 2,\n  \"entries\": [\n")
	for i, key := range sortedKeys(entries) {
		if i != 0 {
			buffer.WriteString(",\n")
		}
		var row bytes.Buffer
		encoder := json.NewEncoder(&row)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(entries[key]); err != nil {
			return fmt.Errorf("baseline entry %s: %w", key.File, err)
		}
		buffer.WriteString("    ")
		buffer.Write(bytes.TrimSuffix(row.Bytes(), []byte("\n")))
	}
	buffer.WriteString("\n  ]\n}\n")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		return fmt.Errorf("baseline %s: %w", path, err)
	}
	return nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type options struct {
	reports, root, baseline string
	update, partial         bool
	packages, shellFiles    stringList
}

func (o options) includes(key findingKey) bool {
	if !o.partial {
		return true
	}
	switch key.Gate {
	case golangciGate:
		return slices.Contains(o.packages, filepath.ToSlash(filepath.Dir(key.File)))
	case shellcheckGate:
		return slices.Contains(o.shellFiles, key.File)
	default:
		return false
	}
}

func differences(actual map[findingKey]int, entries map[findingKey]entry, o options) (map[findingKey]int, map[findingKey]int) {
	added, removed := make(map[findingKey]int), make(map[findingKey]int)
	for key, count := range actual {
		if o.includes(key) && count > entries[key].Count {
			added[key] = count - entries[key].Count
		}
	}
	// Partial package graphs can suppress cross-package clone and reachability reports.
	if !o.partial {
		for key, item := range entries {
			if item.Count > actual[key] {
				removed[key] = item.Count - actual[key]
			}
		}
	}
	return added, removed
}

func printFindings(out io.Writer, label string, values map[findingKey]int) {
	for _, key := range sortedKeys(values) {
		_, _ = fmt.Fprintf(out, "%s: %s %s [%s] %s (count %d)\n", label, key.Gate, key.File, key.Rule, key.Text, values[key])
		if key.Function != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", key.Function)
		}
		if key.Source != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", key.Source)
		}
	}
}

func gateCount(values map[findingKey]int, gate string, o options) int {
	count := 0
	for key, n := range values {
		if (gate == "" || key.Gate == gate) && o.includes(key) {
			count += n
		}
	}
	return count
}

func check(o options, out, diagnostic io.Writer) (int, error) {
	actual, err := collect(o.reports, o.root)
	if err != nil {
		return 2, err
	}
	entries, err := readBaseline(o.baseline)
	if err != nil {
		return 2, err
	}
	added, removed := differences(actual, entries, o)
	printFindings(diagnostic, "NEW", added)
	if o.update && len(added) > 0 {
		_, _ = fmt.Fprintln(diagnostic, "REFUSED: --update-baseline cannot add findings; no baseline was changed")
	}
	if o.update && len(added) == 0 {
		if err := reduceBaseline(o.baseline, entries, actual, removed); err != nil {
			return 2, err
		}
		_, _ = fmt.Fprintf(out, "baseline: removed %d findings; added 0\n", gateCount(removed, "", options{}))
	}
	if !o.update {
		printFindings(diagnostic, "STALE (run scripts/gates.sh --update-baseline)", removed)
	}
	printTotals(out, actual, added, removed, o)
	if len(added) > 0 || (len(removed) > 0 && !o.update) {
		return 1, nil
	}
	return 0, nil
}

func reduceBaseline(path string, entries map[findingKey]entry, actual, removed map[findingKey]int) error {
	if len(removed) == 0 {
		return nil
	}
	for key := range removed {
		if actual[key] == 0 {
			delete(entries, key)
			continue
		}
		item := entries[key]
		item.Count = actual[key]
		entries[key] = item
	}
	return writeBaseline(path, entries)
}

func printTotals(out io.Writer, actual, added, removed map[findingKey]int, o options) {
	for _, gate := range gateNames {
		if o.partial && gate != golangciGate && gate != shellcheckGate {
			continue
		}
		newCount, stale := gateCount(added, gate, o), gateCount(removed, gate, o)
		status := "PASS"
		if newCount > 0 || (stale > 0 && !o.update) {
			status = "FAIL"
		}
		_, _ = fmt.Fprintf(out, "%s: %s (%d findings, %d new, %d stale)\n", gate, status, gateCount(actual, gate, o), newCount, stale)
	}
}

func main() {
	var o options
	flag.StringVar(&o.reports, "reports", "", "tool report directory")
	flag.StringVar(&o.root, "root", ".", "repository root")
	flag.StringVar(&o.baseline, "baseline", ".gates/baseline.json", "reasoned baseline")
	flag.BoolVar(&o.update, "update-baseline", false, "remove fixed findings, refusing additions")
	flag.BoolVar(&o.partial, "partial", false, "check only selected packages and shell files")
	flag.Var(&o.packages, "package", "selected package directory; repeatable")
	flag.Var(&o.shellFiles, "shell-file", "selected shell file; repeatable")
	flag.Parse()
	if o.reports == "" || flag.NArg() > 0 || (o.partial && o.update) {
		fmt.Fprintln(os.Stderr, "gates: reports required; partial scans cannot update the baseline")
		os.Exit(2)
	}
	root, err := filepath.Abs(o.root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gates: root: %v\n", err)
		os.Exit(2)
	}
	o.root = root
	status, err := check(o, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gates: invalid baseline or tool report: %v\n", err)
	}
	os.Exit(status)
}
