package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/config"
)

// TestEveryResolvedPlatformHasAHandler is the guard against the resolver and
// the registry drifting apart. A platform internal/sourceidentity can name with
// nothing registered under it fails a real share while every unit test in the
// repository still passes, because each handler is well tested on its own.
//
// The names are read out of the resolver's source rather than listed here: a
// list in this file would be one more thing to forget, and forgetting it is
// exactly the drift this is checking.
func TestEveryResolvedPlatformHasAHandler(t *testing.T) {
	registry, err := newRegistry(config.Config{}, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	names := resolverPlatforms(t)
	// A scan that finds nothing would pass silently and check nothing.
	if len(names) < 10 {
		t.Fatalf("only %d platform names were read out of the resolver: %v", len(names), names)
	}

	for _, name := range names {
		handler, ok := registry.Get(name)
		if !ok {
			t.Errorf("the resolver names %q and no handler is registered for it", name)
			continue
		}
		// The fallback answers for every hostname, so reaching it is not proof
		// of a registration: a named platform must reach its own handler.
		if handler.Platform() != name {
			t.Errorf("the resolver names %q but it reaches the %q handler",
				name, handler.Platform())
		}
	}
}

// resolverPlatforms reads every platform name internal/sourceidentity can put
// on an identity, other than the bare hostname a generic link carries. Three
// shapes produce one: a Platform field in a SourceIdentity literal, a return in
// placePlatform, and the second half of a placeHostTokens entry.
func resolverPlatforms(t *testing.T) []string {
	t.Helper()

	const dir = "../../internal/sourceidentity"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	record := func(expression ast.Expr) {
		literal, ok := expression.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value != "" {
			found[value] = true
		}
	}

	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "Platform" {
					record(node.Value)
				}
			case *ast.FuncDecl:
				if node.Name.Name != "placePlatform" {
					return true
				}
				ast.Inspect(node.Body, func(inner ast.Node) bool {
					if returns, ok := inner.(*ast.ReturnStmt); ok {
						for _, result := range returns.Results {
							record(result)
						}
					}
					return true
				})
			case *ast.ValueSpec:
				if len(node.Names) != 1 || node.Names[0].Name != "placeHostTokens" {
					return true
				}
				for _, value := range node.Values {
					list, ok := value.(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, element := range list.Elts {
						pair, ok := element.(*ast.CompositeLit)
						if ok && len(pair.Elts) == 2 {
							record(pair.Elts[1])
						}
					}
				}
			}
			return true
		})
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
