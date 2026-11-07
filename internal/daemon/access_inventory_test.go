package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestScopedDenialInventoryCoversActualControlOwners(t *testing.T) {
	constants := protocolTypeConstants(t)
	denied := make(map[string]bool)
	for _, name := range deniedScopedTypes {
		denied[name] = true
	}
	for name := range deniedScopedActions {
		denied[name] = true
	}
	for _, path := range []string{"server.go", "relay.go", "lifecycle.go", "recovery.go", "copy.go", "apps.go", "agent_context.go", "services.go", "certificates.go", "wake.go", "state_requests.go", "update.go"} {
		checkScopedOwnerTypes(t, path, constants, denied)
	}
}
func checkScopedOwnerTypes(t *testing.T, path string, constants map[string]string, denied map[string]bool) {
	t.Helper()
	source, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(source, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := selector.X.(*ast.Ident)
		if !ok || owner.Name != "protocol" || !strings.HasPrefix(selector.Sel.Name, "Type") {
			return true
		}
		name := constants[selector.Sel.Name]
		if name != "" && !denied[name] && name != "access.read" && name != "access.read.result" && name != "access.denied" {
			t.Errorf("actual owner %s references %s with no framed denial case", path, name)
		}
		return true
	})
}

func protocolTypeConstants(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("../protocol/*.go")
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]string)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		collectProtocolTypes(source, result)
	}
	return result
}
func collectProtocolTypes(source *ast.File, result map[string]string) {
	ast.Inspect(source, func(node ast.Node) bool {
		value, ok := node.(*ast.ValueSpec)
		if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
			return true
		}
		literal, ok := value.Values[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(literal.Value)
		if err == nil {
			result[value.Names[0].Name] = text
		}
		return true
	})
}
func TestScopedDenialInventoryCoversActualCompoundActions(t *testing.T) {
	for _, owner := range []struct{ path, function, parent string }{
		{"../transfer/store.go", "Handle", "copy.request"},
		{"../apps/origin.go", "Handle", "app.request"},
		{"../apps/registry.go", "apply", "app.registry"},
		{"update.go", "handle", "update.control"},
	} {
		checkCompoundActions(t, owner.path, owner.function, owner.parent)
	}
}
func checkCompoundActions(t *testing.T, path, function, parent string) {
	t.Helper()
	expected := make(map[string]bool)
	for _, action := range deniedScopedActions[parent] {
		expected[action] = true
	}
	source, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range source.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function {
			continue
		}
		checkCompoundBody(t, fn.Body, expected, parent)
	}
}
func checkCompoundClause(t *testing.T, clause *ast.CaseClause, expected map[string]bool, parent string) {
	t.Helper()
	for _, expression := range clause.List {
		literal, ok := expression.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			continue
		}
		action, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !expected[action] {
			t.Errorf("actual %s action %s has no framed denial case", parent, action)
		}
	}
}

func checkCompoundBody(t *testing.T, body *ast.BlockStmt, expected map[string]bool, parent string) {
	t.Helper()
	ast.Inspect(body, func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)
		if !ok {
			return true
		}
		checkCompoundClause(t, clause, expected, parent)
		return true
	})
}
