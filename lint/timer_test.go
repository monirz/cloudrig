// Package lint holds build-time invariants cheaper to enforce as a test than
// as a code review habit.
package lint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbidden lists the time identifiers that leak wall-clock time in. Now,
// Since and Until read the real clock; the rest schedule against it.
var forbidden = map[string]string{
	"Now":       "use Clock.Now",
	"Since":     "use Clock.Now().Sub",
	"Until":     "use Clock.Now() and Sub",
	"Sleep":     "schedule with Clock.AfterFunc",
	"After":     "schedule with Clock.AfterFunc",
	"AfterFunc": "use Clock.AfterFunc",
	"Tick":      "schedule with Clock.AfterFunc",
	"NewTimer":  "use Clock.AfterFunc",
	"NewTicker": "use Clock.AfterFunc",
}

// exempt lists directories allowed to touch the real clock.
var exempt = []string{
	"core/clock",
}

func TestNoDirectTimeUse(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skipDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests may use real time: they are not the thing under emulation.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checkFile(t, path, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

func skipDir(rel, name string) bool {
	if rel == "." {
		return false
	}
	if strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" {
		return true
	}
	for _, e := range exempt {
		if rel == filepath.FromSlash(e) {
			return true
		}
	}
	return false
}

func checkFile(t *testing.T, path, rel string) {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Errorf("%s: parse: %v", rel, err)
		return
	}

	// Resolve the import's local name; an alias would slip a name check.
	local := ""
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "time" {
			continue
		}
		local = "time"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" {
		return
	}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != local {
			return true
		}
		// A local shadowing the package name resolves to an object.
		if ident.Obj != nil {
			return true
		}
		fix, bad := forbidden[sel.Sel.Name]
		if !bad {
			return true
		}
		pos := fset.Position(sel.Pos())
		t.Errorf("%s:%d:%d: %s.%s is forbidden outside core/clock: %s",
			rel, pos.Line, pos.Column, local, sel.Sel.Name, fix)
		return true
	})
}

// moduleRoot walks up to go.mod, so the check covers the whole module.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}
