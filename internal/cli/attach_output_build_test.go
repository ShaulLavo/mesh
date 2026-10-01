//go:build !windows

package cli

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAttachOutputPollingIsLinuxOnly(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			ctx := build.Default
			ctx.GOOS = platform
			ctx.GOARCH = "arm64"
			pkg, err := ctx.ImportDir(".", 0)
			if err != nil {
				t.Fatal(err)
			}
			features := attachmentBuildFeatures(t, pkg)
			for _, feature := range []string{"poll", "TestAttachCancellationUnblocksFullOutputAndRestoresPTY", "TestAttachRestorationToFullOutputIsBounded"} {
				if features[feature] != (platform == "linux") {
					t.Errorf("%s build includes %s = %t; want %t", platform, feature, features[feature], platform == "linux")
				}
			}
			for _, feature := range []string{"TestAttachNeverMakesSharedOutputNonblocking", "TestAttachDoesNotMakeConcurrentPipeWriterFailWithEAGAIN", "TestAttachPTYControlCCancelsEstablishment", "TestCancelledAttachmentBindingDependsOnAcknowledgement"} {
				if !features[feature] {
					t.Errorf("%s build lost portable regression %s", platform, feature)
				}
			}
		})
	}
}

func attachmentBuildFeatures(t *testing.T, pkg *build.Package) map[string]bool {
	t.Helper()
	features := make(map[string]bool)
	for _, name := range slices.Concat(pkg.GoFiles, pkg.TestGoFiles) {
		if !strings.HasPrefix(name, "attach_") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				features[n.Name.Name] = true
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				owner, ok := selector.X.(*ast.Ident)
				if ok && owner.Name == "unix" && selector.Sel.Name == "Poll" && strings.HasPrefix(name, "attach_output") {
					features["poll"] = true
				}
			}
			return true
		})
	}
	return features
}
