// Package archtest enforces the hexagonal dependency rule:
//
//   - core/domain imports nothing from stellwerk, no I/O packages and no
//     expression language (those are engines behind domain.Engine)
//   - decision engines import only the domain, their library and no I/O
//   - core/ports imports only core/domain
//   - core/usecase imports only core/domain and core/ports
//   - core/ports/portstest (the test kit) imports only the core
//   - adapters never import each other across the in/out boundary
//     (an inbound adapter must not call a target directly)
//   - nothing but bootstrap and cmd imports bootstrap or every adapter
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const module = "github.com/johnnycube/stellwerk/"

func root(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// imports returns import path -> files for all non-test Go files under dir.
// A trailing "/." on dir means this package only, no subpackages.
func imports(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	flat := strings.HasSuffix(dir, string(filepath.Separator)+".")
	dir = strings.TrimSuffix(dir, string(filepath.Separator)+".")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && flat && path != dir {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			out[p] = append(out[p], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func check(t *testing.T, dir string, allowed func(string) bool) {
	t.Helper()
	path := filepath.Join(root(t), dir)
	if strings.HasSuffix(dir, "/.") {
		path += string(filepath.Separator) + "." // keep the "this package only" marker
	}
	for imp, files := range imports(t, path) {
		if !allowed(imp) {
			rel, _ := filepath.Rel(root(t), files[0])
			t.Errorf("%s imports %s, which breaks the dependency rule", rel, imp)
		}
	}
}

var ioPackages = []string{"net/http", "net/", "os", "database/sql", "go.yaml.in/yaml/v3", "maunium.net/go/mautrix"}

func isIO(p string) bool {
	for _, io := range ioPackages {
		if p == io || (strings.HasSuffix(io, "/") && strings.HasPrefix(p, io)) || strings.HasPrefix(p, io+"/") {
			return true
		}
	}
	return false
}

func TestDomainIsPure(t *testing.T) {
	check(t, "internal/core/domain", func(p string) bool {
		return !strings.HasPrefix(p, module) && !isIO(p) && !isEngineLib(p)
	})
}

// Expression languages are adapters behind domain.Engine. The domain must
// not depend on one, so a second engine is a plug-in, not a rewrite.
var engineLibs = []string{"cel.dev/cel-go", "github.com/expr-lang/", "github.com/itchyny/gojq", "github.com/dop251/goja"}

func isEngineLib(p string) bool {
	for _, l := range engineLibs {
		if strings.HasPrefix(p, l) {
			return true
		}
	}
	return false
}

// Decision engines are pure: they may use the domain and their language
// library, nothing that does I/O and no other adapter.
func TestEnginesArePure(t *testing.T) {
	check(t, "internal/adapters/engine", func(p string) bool {
		if strings.HasPrefix(p, module) {
			return p == module+"internal/core/domain" || strings.HasPrefix(p, module+"internal/adapters/engine")
		}
		return !isIO(p)
	})
}

func TestPortsDependOnlyOnDomain(t *testing.T) {
	check(t, "internal/core/ports/.", func(p string) bool {
		if strings.HasPrefix(p, module) {
			return p == module+"internal/core/domain"
		}
		return !isIO(p)
	})
}

func TestUseCasesDependOnlyOnCore(t *testing.T) {
	check(t, "internal/core/usecase", func(p string) bool {
		if strings.HasPrefix(p, module) {
			return p == module+"internal/core/domain" || p == module+"internal/core/ports"
		}
		return !isIO(p)
	})
}

func TestTestKitDependsOnlyOnCore(t *testing.T) {
	check(t, "internal/core/ports/portstest", func(p string) bool {
		if strings.HasPrefix(p, module) {
			return p == module+"internal/core/domain" || p == module+"internal/core/ports"
		}
		return !isIO(p)
	})
}

// Driving adapters reach the core only through use case ports: they must not
// import the usecase package (which would tie them to implementations) except
// the HTTP adapter's tests, which are not checked here.
func TestDrivingAdaptersUsePortsNotImplementations(t *testing.T) {
	check(t, "internal/adapters/in", func(p string) bool {
		return p != module+"internal/core/usecase"
	})
}

// Every driving port must be implemented by a use case.
func TestEveryDrivingPortHasAUseCase(t *testing.T) {
	ports := interfaces(t, "internal/core/ports/in.go")
	impls := assertions(t, "internal/core/usecase")
	for _, p := range ports {
		if !impls[p] {
			t.Errorf("driving port %s has no use case (missing `var _ ports.%s = ...` in internal/core/usecase)", p, p)
		}
	}
	if len(ports) < 4 {
		t.Errorf("found only %v", ports)
	}
}

func TestInboundAdaptersDoNotReachOutbound(t *testing.T) {
	check(t, "internal/adapters/in", func(p string) bool {
		return !strings.HasPrefix(p, module+"internal/adapters/out") && !strings.HasPrefix(p, module+"internal/bootstrap") && !strings.HasPrefix(p, module+"internal/config")
	})
}

func TestOutboundAdaptersDoNotReachInbound(t *testing.T) {
	check(t, "internal/adapters/out", func(p string) bool {
		return !strings.HasPrefix(p, module+"internal/adapters/in") && !strings.HasPrefix(p, module+"internal/bootstrap") &&
			!strings.HasPrefix(p, module+"internal/config") && !strings.HasPrefix(p, module+"internal/core/usecase")
	})
}

func TestOnlyCmdUsesBootstrap(t *testing.T) {
	check(t, "internal", func(p string) bool {
		return !strings.HasPrefix(p, module+"cmd")
	})
	for imp, files := range imports(t, filepath.Join(root(t), "internal")) {
		if imp == module+"internal/bootstrap" {
			t.Errorf("%v import bootstrap, only cmd may", files)
		}
	}
}

// interfaces lists the interface types declared in a file.
func interfaces(t *testing.T, file string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root(t), file), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range g.Specs {
			if ts, ok := s.(*ast.TypeSpec); ok {
				if _, ok := ts.Type.(*ast.InterfaceType); ok {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	return out
}

// assertions finds `var _ ports.X = ...` compile-time checks in a package.
func assertions(t *testing.T, dir string) map[string]bool {
	out := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(root(t), dir, "*.go"))
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "_" {
				return true
			}
			if sel, ok := vs.Type.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "ports" {
					out[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	return out
}
