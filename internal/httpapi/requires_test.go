package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The route table's `requires` list only protects what it names. Startup
// validation catches a declared dependency that is nil and a name that is not a
// Deps field, but nothing stops a handler reaching for a dependency the route
// never declared, and that is exactly how collections and lifecycle shipped
// answering 500 from every one of their seventeen routes.
//
// So this reads the source: which Deps fields does each handler actually touch,
// and did its route say so. Adding a dependency to a handler and forgetting the
// route now fails here rather than in production.
func TestEveryDependencyAHandlerTouchesIsDeclared(t *testing.T) {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	touched := map[string]map[string]bool{}
	var funcs []*ast.FuncDecl
	for _, p := range pkg {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Recv != nil {
					funcs = append(funcs, fn)
				}
			}
		}
	}
	for _, fn := range funcs {
		fields := map[string]bool{}
		collectDepsFields(fn.Body, fields)
		if len(fields) > 0 {
			touched[fn.Name.Name] = fields
		}
	}

	for handler, declared := range declaredRequires(t) {
		for field := range touched[handler] {
			if !declared[field] {
				t.Errorf("%s reaches for Deps.%s but its route does not require it", handler, field)
			}
		}
	}
}

// collectDepsFields records every `.deps.<Field>` selector under a node.
func collectDepsFields(node ast.Node, into map[string]bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		outer, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := outer.X.(*ast.SelectorExpr)
		if ok && inner.Sel.Name == "deps" {
			into[outer.Sel.Name] = true
		}
		return true
	})
}

// declaredRequires maps a handler method name to the dependencies its route
// declares, read out of the route table's source rather than from the values,
// because the handler is a func value by the time the table is built.
func declaredRequires(t *testing.T) map[string]map[string]bool {
	t.Helper()
	source, err := readRoutesSource()
	if err != nil {
		t.Fatalf("reading the route table: %v", err)
	}

	// Each entry is `public(...)` or `bearer(...)` on one line: the handler is
	// `s.handleSomething` and the requires are the trailing string literals.
	entry := regexp.MustCompile(`(?m)^\s*(public|bearer)\((.*)\),\s*$`)
	handlerName := regexp.MustCompile(`s\.(\w+)`)
	literal := regexp.MustCompile(`"(\w+)"`)

	out := map[string]map[string]bool{}
	for _, line := range entry.FindAllStringSubmatch(source, -1) {
		args := line[2]
		names := handlerName.FindAllStringSubmatch(args, -1)
		if len(names) == 0 {
			continue
		}
		handler := names[len(names)-1][1]

		declared := map[string]bool{"Logger": true, "Auth": true, "Now": true}
		// Skip the operation id, which is the only other bare string literal.
		for i, lit := range literal.FindAllStringSubmatch(args, -1) {
			if i == 0 {
				continue
			}
			declared[lit[1]] = true
		}
		if out[handler] == nil {
			out[handler] = declared
		} else {
			for k := range declared {
				out[handler][k] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no routes parsed out of the table")
	}
	return out
}

func readRoutesSource() (string, error) {
	data, err := os.ReadFile("routes.go")
	return string(data), err
}
