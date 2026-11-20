package readers_test

import (
	"context"
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
	"time"

	"github.com/full-chaos/dev-health-go/latestrow"
	"github.com/full-chaos/dev-health-go/readers"
)

// The census of work_unit_investments version picks has two halves.
//
// The rendered check runs each registered builder (the function production
// calls) against a capturing client and reads the SQL it sends: every scope
// that reads the table must pick each version column exactly once through
// latestrow, and hold no other pick, dedupe or computed_at order.
//
// The inventory parses every non-test Go file of the module and finds each
// string literal that names the table. Each one must sit inside a registered
// declaration, so a new reader of the table fails until it is registered (and,
// as a builder, rendered above). SQL assembled from non-literal parts (a table
// name built at run time) is outside what a static inventory can see.

const workUnitTable = "work_unit_investments"

var workUnitVersionColumns = []string{"from_ts", "to_ts", "theme_distribution_json", "subcategory_distribution_json", "structural_evidence_json"}

type workUnitBuilder struct {
	decl   string
	render func(readers.QueryClient, readers.TimeBound) error
}

// workUnitBuilders are the registered production builders that read the
// table: 5 version columns each, 10 sites.
var workUnitBuilders = []workUnitBuilder{
	{"readers/investment_theme.go:ReadTeamThemeMix", func(c readers.QueryClient, b readers.TimeBound) error {
		_, err := readers.ReadTeamThemeMix(context.Background(), c, "org", []string{"team"}, b)
		return err
	}},
	{"readers/investment_project_theme.go:ReadProjectThemeMixWithRowLimit", func(c readers.QueryClient, b readers.TimeBound) error {
		_, err := readers.ReadProjectThemeMixWithRowLimit(context.Background(), c, "org", []string{"linear:p"}, b, readers.DefaultRowLimit)
		return err
	}},
}

// workUnitOtherDecls name the table without reading it: the key itself and
// the declared column and engine contracts.
var workUnitOtherDecls = []string{
	"latestrow/latestrow.go:OrderKey",
	"schema/schema.go:ProductionColumns",
	"schema/schema.go:EngineFull",
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlSpace        = regexp.MustCompile(`\s+`)
	sqlSpaceAround  = regexp.MustCompile(` ?([(),.]) ?`)
	forbiddenInRead = []*regexp.Regexp{
		regexp.MustCompile(`argm(?:ax|in)`),
		regexp.MustCompile(`\bany(?:last|heavy)?(?:_respect_nulls)?\(`),
		regexp.MustCompile(`\bfinal\b`),
		regexp.MustCompile(`\blimit\b[^()]*\bby\b`),
		regexp.MustCompile(`\b(?:row_number|rank|dense_rank|first_value|last_value|nth_value)\(`),
		regexp.MustCompile(`\bover\(`),
		regexp.MustCompile(`computed_at`),
		regexp.MustCompile(`_part`),
	}
)

// normalizeSQL lowercases, drops comments and identifier quotes, and folds
// whitespace, so a respelling of the same SQL reads the same.
func normalizeSQL(s string) string {
	s = sqlBlockComment.ReplaceAllString(s, " ")
	s = sqlLineComment.ReplaceAllString(s, " ")
	s = strings.NewReplacer("`", "", `"`, "").Replace(strings.ToLower(s))
	s = sqlSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(sqlSpaceAround.ReplaceAllString(s, "$1"))
}

// tableReads returns the innermost parenthesized scope around each place the
// normalized statement names the table as a relation (not as the qualifier of
// a column), or the whole statement when no parenthesis encloses it.
func tableReads(sql string) []string {
	var scopes []string
	for i := 0; ; {
		j := strings.Index(sql[i:], workUnitTable)
		if j < 0 {
			return scopes
		}
		pos := i + j
		i = pos + len(workUnitTable)
		if i < len(sql) && sql[i] == '.' {
			continue
		}
		start, end := 0, len(sql)
		depth := 0
		for k := pos - 1; k >= 0; k-- {
			if sql[k] == ')' {
				depth++
			} else if sql[k] == '(' {
				if depth == 0 {
					start = k + 1
					break
				}
				depth--
			}
		}
		if start > 0 {
			depth = 0
			for k := pos; k < len(sql); k++ {
				if sql[k] == '(' {
					depth++
				} else if sql[k] == ')' {
					if depth == 0 {
						end = k
						break
					}
					depth--
				}
			}
		}
		scopes = append(scopes, sql[start:end])
	}
}

// checkRenderedWorkUnitPicks returns the census failures of one rendered
// statement.
func checkRenderedWorkUnitPicks(statement string) []string {
	sql := normalizeSQL(statement)
	reads := tableReads(sql)
	if len(reads) != 1 {
		return []string{"reads " + workUnitTable + " " + strconv.Itoa(len(reads)) + " times, want 1"}
	}
	var failures []string
	scope := reads[0]
	for _, column := range workUnitVersionColumns {
		site := normalizeSQL(latestrow.ArgMax(column) + " AS " + column)
		if n := strings.Count(scope, site); n != 1 {
			failures = append(failures, column+": the latestrow pick appears "+strconv.Itoa(n)+" times, want 1")
		}
		scope = strings.ReplaceAll(scope, site, "")
	}
	for _, forbidden := range forbiddenInRead {
		if hit := forbidden.FindString(scope); hit != "" {
			failures = append(failures, "the read holds a pick or order outside latestrow: "+hit)
		}
	}
	if n := strings.Count(sql, normalizeSQL(latestrow.OrderKey)); n != len(workUnitVersionColumns) {
		failures = append(failures, "the key appears "+strconv.Itoa(n)+" times, want "+strconv.Itoa(len(workUnitVersionColumns)))
	}
	return failures
}

func TestWorkUnitVersionPicksRenderTheSharedKey(t *testing.T) {
	bounds := map[string]readers.TimeBound{
		"unbounded": {},
		"range":     {Active: true, HasStart: true, Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, builder := range workUnitBuilders {
		for name, bound := range bounds {
			client := &fakeClient{}
			if err := builder.render(client, bound); err != nil {
				t.Fatalf("%s (%s): %v", builder.decl, name, err)
			}
			if len(client.queries) != 1 {
				t.Fatalf("%s (%s) ran %d statements, want 1", builder.decl, name, len(client.queries))
			}
			for _, failure := range checkRenderedWorkUnitPicks(client.queries[0].statement) {
				t.Errorf("%s (%s): %s", builder.decl, name, failure)
			}
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
				if err != nil || !strings.Contains(normalizeSQL(value), workUnitTable) {
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
	for _, builder := range workUnitBuilders {
		registered[builder.decl] = true
	}
	for _, decl := range workUnitOtherDecls {
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
			t.Errorf("%s names %s outside the registered builders: register it (a builder also gets the rendered check)", site, workUnitTable)
		}
	}
	for site := range registered {
		if found[site] == 0 {
			t.Errorf("registered %s no longer names %s: drop or fix the registration", site, workUnitTable)
		}
	}
}
