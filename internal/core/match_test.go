package core

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"testing"
)

// TestMatchHasNoEarlyExit reads match in token.go and fails when its loop can
// stop before the last hash, branches on a comparison, or compares without
// crypto/subtle, because the check must take the same time whichever hash
// matches. Behaviour tests pass with an early exit, so the shape is checked here.
func TestMatchHasNoEarlyExit(t *testing.T) {
	f, err := parser.ParseFile(gotoken.NewFileSet(), "token.go", nil, 0)
	if err != nil {
		t.Fatalf("token.go does not parse: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "match" {
			fn = d
		}
	}
	if fn == nil {
		t.Fatal("token.go has no func match, so this test checks nothing. Point it at the function that compares token hashes.")
	}

	loops, compares := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		var body *ast.BlockStmt
		switch n := n.(type) {
		case *ast.RangeStmt:
			body = n.Body
		case *ast.ForStmt:
			body = n.Body
		default:
			return true
		}
		loops++
		ast.Inspect(body, func(m ast.Node) bool {
			switch m := m.(type) {
			case *ast.ReturnStmt:
				t.Errorf("match returns inside its loop, so it can stop before the last hash")
			case *ast.BranchStmt:
				t.Errorf("match has %s inside its loop, so it can stop before the last hash", m.Tok)
			case *ast.IfStmt, *ast.SwitchStmt:
				t.Errorf("match branches inside its loop, so its time depends on which hash matches")
			case *ast.BinaryExpr:
				if m.Op == gotoken.EQL || m.Op == gotoken.NEQ {
					t.Errorf("match compares with %s inside its loop instead of crypto/subtle", m.Op)
				}
			case *ast.SelectorExpr:
				if x, ok := m.X.(*ast.Ident); ok && x.Name == "subtle" && m.Sel.Name == "ConstantTimeCompare" {
					compares++
				}
			}
			return true
		})
		return false
	})
	if loops != 1 || compares != 1 {
		t.Errorf("match has %d loops and %d subtle.ConstantTimeCompare calls in them, want one of each", loops, compares)
	}
}
