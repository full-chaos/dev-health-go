package readers_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The census of work_unit_investments readers is a static inventory: it parses
// every non-test Go file of the module with go/ast and finds each string
// literal that names the table as a whole identifier. Each one must sit inside
// a registered declaration, so a new reader of the table fails until it is
// registered. It does not read the SQL a registered builder renders: the key a
// builder uses is held by the tie cell
// (TestIntegrationWorkUnitVersionSameMillisecondTiePicksTheLaterInsert), and
// SQL assembled from non-literal parts is outside a static inventory.

// workUnitTableToken matches the table name as a whole identifier, so a
// related table (work_unit_investments_rollup) is not a match.
var workUnitTableToken = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_])work_unit_investments(?:$|[^a-z0-9_])`)

// workUnitRegisteredDecls are the declarations allowed to name the table: the
// 2 builders of the 10 version picks, the key itself and the declared column
// and engine contracts.
var workUnitRegisteredDecls = []string{
	"readers/investment_theme.go:ReadTeamThemeMix",
	"readers/investment_project_theme.go:ReadProjectThemeMixWithRowLimit",
	"latestrow/latestrow.go:OrderKey",
	"schema/schema.go:ProductionColumns",
	"schema/schema.go:EngineFull",
}

func namesWorkUnitTable(s string) bool {
	return workUnitTableToken.MatchString(s)
}

func TestWorkUnitTableTokenMatch(t *testing.T) {
	cases := map[string]bool{
		"SELECT x FROM work_unit_investments":                                  true,
		"FROM\n    work_unit_investments GROUP BY 1":                           true,
		"select x from `default`.`work_unit_investments` final":                true,
		"FROM WORK_UNIT_INVESTMENTS":                                           true,
		"work_unit_investments":                                                true,
		"work_unit_investments.computed_at":                                    true,
		"SELECT x FROM work_unit_investments_rollup":                           false,
		"SELECT x FROM my_work_unit_investments":                               false,
		"SELECT work_unit_investments2 FROM t":                                 false,
		"SELECT argMax(team_id, computed_at) FROM work_item_team_attributions": false,
	}
	for input, want := range cases {
		if got := namesWorkUnitTable(input); got != want {
			t.Errorf("namesWorkUnitTable(%q) = %v, want %v", input, got, want)
		}
	}
}

// workUnitTableLiterals returns "file:declaration" for every string literal
// in the module's non-test Go files that names the table.
func workUnitTableLiterals(t *testing.T, root string) map[string]int {
	t.Helper()
	found := map[string]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil || !namesWorkUnitTable(value) {
					return true
				}
				found[filepath.ToSlash(rel)+":"+declName(decl, lit)]++
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func declName(decl ast.Decl, lit *ast.BasicLit) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) == 1 {
			return exprName(d.Recv.List[0].Type) + "." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && vs.Pos() <= lit.Pos() && lit.End() <= vs.End() {
				names := make([]string, len(vs.Names))
				for i, name := range vs.Names {
					names[i] = name.Name
				}
				return strings.Join(names, ",")
			}
		}
	}
	return "?"
}

func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return exprName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return exprName(x.X)
	}
	return "?"
}

func TestWorkUnitVersionKeyCensus(t *testing.T) {
	registered := map[string]bool{}
	for _, decl := range workUnitRegisteredDecls {
		registered[decl] = true
	}
	found := workUnitTableLiterals(t, "..")
	var sites []string
	for site := range found {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	for _, site := range sites {
		if !registered[site] {
			t.Errorf("%s names work_unit_investments outside the registered declarations: register it", site)
		}
	}
	for _, decl := range workUnitRegisteredDecls {
		if found[decl] == 0 {
			t.Errorf("registered %s no longer names work_unit_investments: drop or fix the registration", decl)
		}
	}
}
