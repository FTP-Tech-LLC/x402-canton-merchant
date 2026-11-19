package echo_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// echo's router is not safe for registration during traffic, and iterating its
// route map while a route is added aborts the process. The adapter's request path
// (echo.go) must therefore never call Routes() or Reverse(): it asks the router
// (Find), as echo does per request. check.go runs once at startup and may.
func TestZ2AdapterNeverReadsEchosRouteMapWhileServing(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "echo.go", nil, 0)
	require.NoError(t, err)
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				require.NotContains(t, []string{"Routes", "Reverse"}, sel.Sel.Name, "echo.go calls %s() on the request path", sel.Sel.Name)
			}
		}
		return true
	})
}
