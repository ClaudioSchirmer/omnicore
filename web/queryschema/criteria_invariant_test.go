package queryschema

// The read vocabulary drifted for years in the only way it could: five surfaces
// each built their own ReadCriteria, so every rule existed five times and the
// copies disagreed one edit at a time. BuildCriteria is now the single place a
// wire becomes a criteria — but "single" is a property nothing enforced, and a
// sixth surface, or a shortcut on an existing one, would restore the old shape
// without anyone noticing until a consumer hit the difference.
//
// This test is the enforcement. It reads the source of every web surface and
// fails when one of them assembles a criteria of its own.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// criteriaAssembler is the one file allowed to populate a ReadCriteria from a
// decoded request. Everything else decodes its own wire and hands the result
// over.
const criteriaAssembler = "web/queryschema/criteria.go"

// TestInvariant_OnlyTheAssemblerBuildsCriteria fails when any file under web/
// other than the assembler writes a queries.ReadCriteria literal WITH FIELDS.
//
// A zero literal is not a construction — `return queries.ReadCriteria{}, err` is
// how a surface returns nothing on a refusal — so only a literal that sets
// something counts.
func TestInvariant_OnlyTheAssemblerBuildsCriteria(t *testing.T) {
	var offenders []string
	walkGoFiles(t, webRoot(t), func(path string, file *ast.File, fset *token.FileSet) {
		if strings.HasSuffix(filepath.ToSlash(path), criteriaAssembler) {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) == 0 {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ReadCriteria" {
				return true
			}
			offenders = append(offenders, fset.Position(lit.Pos()).String())
			return true
		})
	})
	if len(offenders) > 0 {
		t.Fatalf(
			"a surface is assembling its own ReadCriteria:\n  %s\n\n"+
				"Decode the wire into a queryschema.Read and call BuildCriteria. What the endpoint\n"+
				"accepts is the Request DTO's answer, and it has to be the same answer on every wire;\n"+
				"a surface owns only how ITS wire spells a value and how it renders a refusal.\n"+
				"If this literal is genuinely part of the assembler, it belongs in %s.",
			strings.Join(offenders, "\n  "), criteriaAssembler)
	}
}

// webRoot returns the absolute path of the web/ tree, found by walking up from
// this package.
func webRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return filepath.Dir(dir) // web/queryschema → web
}

// walkGoFiles parses every non-test .go file under root and hands it to visit.
// Test files are skipped: a fixture may legitimately build a criteria to hand
// a reader.
func walkGoFiles(t *testing.T, root string, visit func(string, *ast.File, *token.FileSet)) {
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
