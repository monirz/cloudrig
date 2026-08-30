// Package functions builds and runs Go Cloud Functions as child processes.
//
// There is no Docker and no buildpack: cloudrig generates a main that imports
// the function's package, compiles it with the toolchain already present, and
// serves it. Go only, for now.
package functions

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
)

// DefaultProject and DefaultLocation stand in when a caller does not say.
// Real gcloud always sends both in the resource path, so these only matter for
// the short URL form and for a CLI invocation without --project.
const (
	DefaultProject  = "cloudrig-local"
	DefaultLocation = "us-central1"
)

// Function names a deployable HTTP function.
type Function struct {
	// Project and Location complete the identity. Empty means the defaults.
	Project  string
	Location string

	// Name is the function id, the last segment of its resource name.
	Name string

	// Source is the directory holding the function's code.
	Source string

	// Runtime is a gcloud runtime identifier. Empty means detect from Source.
	Runtime Runtime

	// EntryPoint is the exported handler to serve.
	EntryPoint string

	// Watch redeploys the function when its source changes.
	Watch bool

	// Trigger runs the function on an event instead of an HTTP request.
	Trigger EventTrigger
}

// ResourceName is projects/P/locations/L/functions/F, the identity both API
// versions address a function by.
func (f Function) ResourceName() string {
	return ResourceName(f.Project, f.Location, f.Name)
}

// ResourceName builds a function resource name, filling in defaults.
func ResourceName(project, location, name string) string {
	if project == "" {
		project = DefaultProject
	}
	if location == "" {
		location = DefaultLocation
	}
	return "projects/" + project + "/locations/" + location + "/functions/" + name
}

// resolve fills in whatever the caller left blank and returns the launcher.
func (f *Function) resolve() (launcher, error) {
	if f.Project == "" {
		f.Project = DefaultProject
	}
	if f.Location == "" {
		f.Location = DefaultLocation
	}
	switch {
	case f.Name == "":
		return nil, fmt.Errorf("function name is empty")
	case strings.ContainsAny(f.Name, "/?#"):
		return nil, fmt.Errorf("function name %q contains a path character", f.Name)
	case strings.ContainsAny(f.Project, "/?#"), strings.ContainsAny(f.Location, "/?#"):
		return nil, fmt.Errorf("function %s: project and location must not contain path characters", f.Name)
	case f.Source == "":
		return nil, fmt.Errorf("function %s: no source directory given", f.Name)
	}
	if !exists(f.Source) {
		return nil, fmt.Errorf("function %s: source %s: no such file or directory", f.Name, f.Source)
	}

	if f.Runtime == "" {
		detected, err := DetectRuntime(f.Source)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		f.Runtime = detected
	}
	l, ok := runtimes[f.Runtime]
	if !ok {
		return nil, fmt.Errorf("function %s: unknown runtime %q; known: %s",
			f.Name, f.Runtime, strings.Join(KnownRuntimes(), ", "))
	}

	if f.EntryPoint == "" {
		// Only Go can be inspected without running anything, so detection is
		// the Go launcher's business rather than a general capability.
		if _, isGo := l.(goLauncher); !isGo {
			return nil, fmt.Errorf("function %s: %s needs an explicit entry point", f.Name, f.Runtime)
		}
		entry, err := DetectEntryPoint(f.Source)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", f.Name, err)
		}
		f.EntryPoint = entry
	}
	return l, nil
}

// EntryPoints lists the exported func(http.ResponseWriter, *http.Request) in
// dir, sorted, so a caller can detect or report the candidates.
func EntryPoints(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nonTestGoFile, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", dir, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go package in %s", dir)
	}

	var found []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Recv == nil && ast.IsExported(fn.Name.Name) && isHTTPHandler(fn.Type) {
					found = append(found, fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

// DetectEntryPoint picks the only exported handler in dir, so the common case
// needs no --entry-point at all.
func DetectEntryPoint(dir string) (string, error) {
	found, err := EntryPoints(dir)
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%s has no exported func(http.ResponseWriter, *http.Request)", dir)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%s has %d entry points (%s); pass --entry-point",
		dir, len(found), strings.Join(found, ", "))
}

// checkEntryPoint verifies the entry point before codegen, so a typo is a clear
// error rather than a compile failure in code the user never wrote.
func checkEntryPoint(dir, entry string) error {
	found, err := EntryPoints(dir)
	if err != nil {
		return err
	}
	for _, name := range found {
		if name == entry {
			return nil
		}
	}
	if len(found) == 0 {
		return fmt.Errorf("%s has no exported func(http.ResponseWriter, *http.Request)", dir)
	}
	return fmt.Errorf("no entry point %q in %s; found %s", entry, dir, strings.Join(found, ", "))
}

// isHTTPHandler reports whether the signature is
// func(http.ResponseWriter, *http.Request) with no results.
func isHTTPHandler(t *ast.FuncType) bool {
	if t.Results != nil && len(t.Results.List) > 0 {
		return false
	}
	if t.Params == nil || len(t.Params.List) != 2 {
		return false
	}
	return typeName(t.Params.List[0].Type) == "http.ResponseWriter" &&
		typeName(t.Params.List[1].Type) == "*http.Request"
}

func typeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return "*" + typeName(v.X)
	case *ast.SelectorExpr:
		return typeName(v.X) + "." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// nonTestGoFile filters _test.go out of a package parse: an entry point in a
// test file would not compile into the shim.
func nonTestGoFile(fi fs.FileInfo) bool {
	return !strings.HasSuffix(fi.Name(), "_test.go")
}
