package core

// A read backing asks one question about a Go field name: does it resolve on
// this view's schema, and to what column. That question had three answers —
// one in the Mongo reader, two on the relational side — and they disagreed, so
// the SAME Request DTO could address a field on one backing and be refused on
// the other. Resolve is now the single answer.
//
// ColumnOf survives because Resolve is built ON it: it is the narrow lookup
// over a schema's own mapped fields, without the managed slots, the ParentID
// projection, or the 1:1 satellites. That narrowness is correct where the write
// path needs it and wrong everywhere a read resolves a name, which is exactly
// the mistake that shipped. This test keeps the read path off it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readPaths are the trees that answer a READ. A name resolved there must go
// through Resolve, so both backings admit the same set.
//
// The criteria compiler used to live in db/command/read and now lives HERE, in
// core, so that the write statements can key on a criteria too. That does not
// weaken this rule and the list did not need a new entry: the compiler resolves
// nothing itself — it consumes a FieldResolver its caller built — so the trees
// that answer a read are still the two below, and they are still where a name
// gets turned into a column.
var readPaths = []string{
	"db/query",
	"db/command/read",
}

// exempt names the read-side files that legitimately use the narrow lookup,
// with the reason. The rule this test enforces is about RESOLVING A NAME THE
// REQUEST SUPPLIED; enumerating a schema's own declarations is a different job,
// and the narrow lookup is the right one for it.
//
// Adding a line here is a claim that the file resolves nothing on a consumer's
// behalf. Make that true before making it short.
var exempt = map[string]string{
	// physicalColumns walks GoFields() — the schema's OWN declared business
	// columns — to hash the view's declared shape. Resolve would fold in the
	// 1:1 satellites and the managed slots, which are not part of that shape
	// and would change every existing hash.
	"db/query/view_hash.go": "hashes a view's declared shape, resolves no request name",
}

// TestInvariant_TheReadPathResolvesThroughResolve fails when a read-side file
// resolves a field name through ColumnOf, the narrow lookup.
func TestInvariant_TheReadPathResolvesThroughResolve(t *testing.T) {
	var offenders []string
	for _, rel := range readPaths {
		root := filepath.Join(infraRoot(t), rel)
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("read path %s does not exist — fix this list, not the test: %v", rel, err)
		}
		walkNonTestGoFiles(t, root, func(path string, file *ast.File, fset *token.FileSet) {
			if isExempt(t, path) {
				return
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ColumnOf" {
					return true
				}
				offenders = append(offenders, fset.Position(call.Pos()).String())
				return true
			})
		})
	}
	if len(offenders) > 0 {
		t.Fatalf(
			"the read path is resolving a field name through ColumnOf:\n  %s\n\n"+
				"Use TableSchema.Resolve. ColumnOf sees only a schema's own mapped fields, so a\n"+
				"managed slot (CreatedAt/UpdatedAt/DeletedAt), the ParentID projection, a sibling\n"+
				"or a shared-base field resolves on one backing and is refused on the other — from\n"+
				"the same Request DTO. Resolve also reports WHOSE row the column is on, which is\n"+
				"what a backing that has to JOIN needs.",
			strings.Join(offenders, "\n  "))
	}
}

// isExempt reports whether path carries a documented exemption. A stale entry
// is a failure of its own: an exemption that no longer names a real file is a
// hole nobody is watching.
func isExempt(t *testing.T, path string) bool {
	t.Helper()
	slashed := filepath.ToSlash(path)
	for rel := range exempt {
		if strings.HasSuffix(slashed, rel) {
			return true
		}
	}
	return false
}

// TestInvariant_EveryExemptionStillNamesAFile keeps the list honest.
func TestInvariant_EveryExemptionStillNamesAFile(t *testing.T) {
	for rel, reason := range exempt {
		if _, err := os.Stat(filepath.Join(infraRoot(t), rel)); err != nil {
			t.Errorf("exemption %q (%s) names no file — delete it", rel, reason)
		}
	}
}

// infraRoot returns the absolute path of the infra/ tree.
func infraRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(dir)) // infra/db/core → infra
}

func walkNonTestGoFiles(t *testing.T, root string, visit func(string, *ast.File, *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		visit(path, file, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
