package readers_test

// Every reader that bounds its statement with LIMIT must order the OUTER query
// by a total order. Without it ClickHouse may return a different N rows per
// call when more than N rows match, so the same question can get different
// rows downstream. An ORDER BY inside a subquery or inside a window OVER (...)
// does not order the rows the LIMIT cuts, so only an ORDER BY at parenthesis
// depth 0, before the LIMIT, counts.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

type topLevelClauses struct {
	orderBy  []string
	orderAt  []int
	limitAt  int
	hasUnion bool
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// scanTopLevel walks statement once, skipping string literals, quoted
// identifiers and comments, and records ORDER BY / LIMIT / UNION only where
// the parenthesis depth is zero. orderBy holds the text each depth-0 ORDER BY
// runs over, up to the next depth-0 LIMIT, SETTINGS or the end.
func scanTopLevel(statement string) topLevelClauses {
	out := topLevelClauses{limitAt: -1}
	depth := 0
	orderStart := -1
	closeOrder := func(end int) {
		if orderStart >= 0 {
			out.orderBy = append(out.orderBy, strings.TrimSpace(statement[orderStart:end]))
			out.orderAt = append(out.orderAt, orderStart)
			orderStart = -1
		}
	}
	upper := strings.ToUpper(statement)
	for i := 0; i < len(statement); {
		c := statement[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			for i < len(statement) {
				if statement[i] == '\\' {
					i += 2
					continue
				}
				if statement[i] == quote {
					if i+1 < len(statement) && statement[i+1] == quote {
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
		case c == '-' && i+1 < len(statement) && statement[i+1] == '-':
			for i < len(statement) && statement[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(statement) && statement[i+1] == '*':
			end := strings.Index(statement[i+2:], "*/")
			if end < 0 {
				i = len(statement)
			} else {
				i += end + 4
			}
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
		case depth == 0 && isWordByte(c):
			j := i
			for j < len(statement) && isWordByte(statement[j]) {
				j++
			}
			word := upper[i:j]
			switch word {
			case "ORDER":
				k := j
				for k < len(statement) && (statement[k] == ' ' || statement[k] == '\n' || statement[k] == '\t') {
					k++
				}
				if strings.HasPrefix(upper[k:], "BY") && (k+2 == len(statement) || !isWordByte(statement[k+2])) {
					closeOrder(i)
					orderStart = k + 2
					j = k + 2
				}
			case "LIMIT":
				closeOrder(i)
				out.limitAt = i
			case "SETTINGS":
				closeOrder(i)
			case "UNION":
				out.hasUnion = true
			}
			i = j
		default:
			i++
		}
	}
	closeOrder(len(statement))
	return out
}

func TestScanTopLevelIgnoresOrderInsideSubqueriesWindowsStringsAndComments(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		statement string
		wantOrder []string
	}{
		{"none", "SELECT a FROM t\nLIMIT 5", nil},
		{"top level", "SELECT a FROM t\nORDER BY a DESC, b\nLIMIT 5", []string{"a DESC, b"}},
		{"subquery only", "SELECT a FROM (SELECT a FROM t ORDER BY a)\nLIMIT 5", nil},
		{"window only", "SELECT a FROM (SELECT a, row_number() OVER (PARTITION BY a ORDER BY b DESC) AS rn FROM t) WHERE rn = 1\nLIMIT 5", nil},
		{"string literal", "SELECT 'ORDER BY x' FROM t\nLIMIT 5", nil},
		{"double-quoted identifier", "SELECT \"ORDER BY x\" FROM t\nLIMIT 5", nil},
		{"backtick identifier", "SELECT `ORDER BY x` FROM t\nLIMIT 5", nil},
		{"escaped quote in string", "SELECT 'it\\'s ORDER BY x' FROM t\nLIMIT 5", nil},
		{"line comment", "SELECT a FROM t -- ORDER BY a\nLIMIT 5", nil},
		{"block comment", "SELECT a FROM t /* ORDER BY a */\nLIMIT 5", nil},
		{"column named like keyword", "SELECT order_by, reorder FROM t\nLIMIT 5", nil},
		{"top level after subquery", "SELECT a FROM (SELECT a FROM t ORDER BY a)\nORDER BY a\nLIMIT 5", []string{"a"}},
		{"settings ends the order", "SELECT a FROM t\nORDER BY a\nSETTINGS max_threads = 1", []string{"a"}},
	} {
		got := scanTopLevel(tt.statement)
		if strings.Join(got.orderBy, "|") != strings.Join(tt.wantOrder, "|") {
			t.Errorf("%s: orderBy = %q, want %q", tt.name, got.orderBy, tt.wantOrder)
		}
	}
	if !scanTopLevel("SELECT 1 UNION ALL SELECT 2 LIMIT 1").hasUnion {
		t.Error("a depth-0 UNION must be reported")
	}
	if scanTopLevel("SELECT a FROM (SELECT 1 UNION ALL SELECT 2)\nLIMIT 1").hasUnion {
		t.Error("a UNION inside a subquery must not be reported")
	}
}

// orderedLimitRead is one call of a reader that builds a LIMIT statement, and
// the exact top-level ORDER BY it must carry. wantOrder pins the key columns,
// so dropping a tie-break column fails here, not only dropping the clause.
type orderedLimitRead struct {
	label     string
	wantOrder string
	run       func(c *fakeClient) error
}

func orderedLimitReads(t *testing.T) map[string][]orderedLimitRead {
	t.Helper()
	ctx := context.Background()
	const org = "org-1"
	ids := []string{"a:1", "b:2"}
	bounds := []readers.TimeBound{{}, {Active: true, End: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	selector := readers.AuthorizationScope{RepositorySelectors: &readers.RepositorySelectorScope{
		Granted: readers.RepositorySelectorSet{All: true},
	}}

	reads := map[string][]orderedLimitRead{}
	add := func(builder, label, wantOrder string, run func(c *fakeClient) error) {
		reads[builder] = append(reads[builder], orderedLimitRead{label: label, wantOrder: wantOrder, run: run})
	}
	addBounded := func(builder, wantOrder string, run func(c *fakeClient, tb readers.TimeBound) error) {
		for i, tb := range bounds {
			tb := tb
			label := "inactive"
			if i == 1 {
				label = "active"
			}
			add(builder, label, wantOrder, func(c *fakeClient) error { return run(c, tb) })
		}
	}

	addBounded("ReadRunStatus", "c.started_at DESC, c.repo_id, c.run_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadRunStatus(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadCICDMetricsDaily", "day DESC, repo_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadCICDMetricsDaily(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadDeploymentStatus", "coalesce(d.started_at, d.deployed_at) DESC, d.repo_id, d.deployment_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadDeploymentStatus(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadDeployMetricsDaily", "day DESC, repo_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadDeployMetricsDaily(ctx, c, org, ids, tb)
		return err
	})
	add("ReadRepositoryIdentityWithRowLimit", "limit 7", "r.id", func(c *fakeClient) error {
		_, err := readers.ReadRepositoryIdentityWithRowLimit(ctx, c, org, ids, 7)
		return err
	})
	add("ReadWorkItemIdentityWithRowLimit", "limit 7", "w.repo_id, w.work_item_id", func(c *fakeClient) error {
		_, err := readers.ReadWorkItemIdentityWithRowLimit(ctx, c, org, ids, 7)
		return err
	})
	add("ReadRepositoryIDsWithRowLimit", "limit 7", "r.id", func(c *fakeClient) error {
		_, err := readers.ReadRepositoryIDsWithRowLimit(ctx, c, org, ids, 7)
		return err
	})
	add("ReadWorkItemRepositoryWithRowLimit", "limit 7", "w.repo_id, w.work_item_id", func(c *fakeClient) error {
		_, err := readers.ReadWorkItemRepositoryWithRowLimit(ctx, c, org, ids, 7)
		return err
	})
	addBounded("ReadIncidents", "i.started_at DESC, i.id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadIncidents(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadRepositoryMetrics", "day DESC, repo_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadRepositoryMetrics(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadTeamMetrics", "day DESC, team_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadTeamMetrics(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadProjectMetricsBreakdown", "p.id, p.provider, tm.team_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadProjectMetricsBreakdown(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadPullRequestState", "p.created_at DESC, p.repo_id, p.number", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadPullRequestState(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadPullRequestReviews", "r.submitted_at DESC, r.repo_id, r.number, r.review_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadPullRequestReviews(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadTeamReadiness", "day DESC, team_id, work_scope_id, provider", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadTeamReadiness(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadProjectReadiness", "p.id, p.provider, ec.work_scope_id, ec.provider, ec.team_key", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadProjectReadiness(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadSourceHealth", "created_at DESC, provider", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadSourceHealth(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadTeamWorkload", "computed_at DESC, team_id, work_scope_id", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadTeamWorkload(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadProjectWorkload", "p.id, p.provider, cf.work_scope_id, cf.has_team, cf.team_key", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadProjectWorkload(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadTeamInvestment", "day DESC, team_id, investment_area, project_stream", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadTeamInvestment(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadProjectInvestment", "p.id, p.provider, p.team_id, im.investment_area, im.project_stream", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadProjectInvestment(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadTeamThemeMix", "team_id, kind, key", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadTeamThemeMix(ctx, c, org, ids, tb)
		return err
	})
	addBounded("ReadProjectThemeMixWithRowLimit", "project_key", func(c *fakeClient, tb readers.TimeBound) error {
		_, err := readers.ReadProjectThemeMixWithRowLimit(ctx, c, org, ids, tb, 7)
		return err
	})

	for _, scope := range []struct {
		label string
		value readers.AuthorizationScope
	}{{"id scope", readers.AuthorizationScope{}}, {"selector scope", selector}} {
		scope := scope
		add("workItemReadStatement", "status "+scope.label, "w.repo_id, w.work_item_id", func(c *fakeClient) error {
			_, err := readers.ReadWorkItemStatusWithScopeAndRowLimit(ctx, c, org, ids, scope.value, readers.Settings{}, 7)
			return err
		})
		add("workItemReadStatement", "title "+scope.label, "w.repo_id, w.work_item_id", func(c *fakeClient) error {
			_, err := readers.ReadWorkItemTitleWithScopeAndRowLimit(ctx, c, org, ids, scope.value, readers.Settings{}, 7)
			return err
		})
		for i, tb := range bounds {
			tb := tb
			add("workItemReadStatement", "completion "+scope.label+" bound "+string(rune('0'+i)), "w.created_at DESC, w.repo_id, w.work_item_id", func(c *fakeClient) error {
				_, err := readers.ReadWorkItemCompletionWithScopeAndRowLimit(ctx, c, org, ids, tb, scope.value, readers.Settings{}, 7)
				return err
			})
		}
	}
	return reads
}

// functionsThatBuildALimit parses the package's non-test source and returns
// every function that calls WithRowLimit. A new reader that bounds a statement
// in a function this test does not know about fails the completeness check
// below instead of slipping past the order guard.
func functionsThatBuildALimit(t *testing.T) []string {
	t.Helper()
	names, err := scanLimitBuilders(".")
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func scanLimitBuilders(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("parse readers source: %w", err)
	}
	pkg, ok := pkgs["readers"]
	if !ok {
		return nil, fmt.Errorf("package readers not found in the source directory; found %d packages", len(pkgs))
	}
	found := map[string]bool{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == "WithRowLimit" {
					found[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errors.New("no function calling WithRowLimit was found; the source scan measured nothing")
	}
	return names, nil
}

// checkTotalTopLevelOrder returns why statement does not carry exactly the
// wanted ORDER BY on the query that carries its LIMIT, or nil.
func checkTotalTopLevelOrder(statement, wantOrder string) error {
	clauses := scanTopLevel(statement)
	if clauses.limitAt < 0 {
		return fmt.Errorf("statement has no top-level LIMIT, so the guard measured nothing: %q", statement)
	}
	if clauses.hasUnion {
		return fmt.Errorf("statement has a top-level UNION, so a trailing ORDER BY orders only its last arm: %q", statement)
	}
	if len(clauses.orderBy) != 1 {
		return fmt.Errorf("top-level ORDER BY clauses = %q, want exactly one before LIMIT: %q", clauses.orderBy, statement)
	}
	if clauses.orderAt[0] > clauses.limitAt {
		return fmt.Errorf("the top-level ORDER BY does not precede the LIMIT: %q", statement)
	}
	if clauses.orderBy[0] != wantOrder {
		return fmt.Errorf("ORDER BY %q, want %q", clauses.orderBy[0], wantOrder)
	}
	return nil
}

func TestTotalOrderCheckRejectsEachPlantedDefect(t *testing.T) {
	t.Parallel()
	if err := checkTotalTopLevelOrder("SELECT a FROM t\nORDER BY a, b\nLIMIT 5", "a, b"); err != nil {
		t.Fatalf("a correct statement was rejected: %v", err)
	}
	for _, tt := range []struct {
		name      string
		statement string
		wantErr   string
	}{
		{"no limit", "SELECT a FROM t\nORDER BY a, b", "no top-level LIMIT"},
		{"no order", "SELECT a FROM t\nLIMIT 5", "exactly one"},
		{"order only in a subquery", "SELECT a FROM (SELECT a FROM t ORDER BY a, b)\nLIMIT 5", "exactly one"},
		{"order only in a window", "SELECT a FROM (SELECT a, row_number() OVER (ORDER BY a, b) AS rn FROM t)\nLIMIT 5", "exactly one"},
		{"order after the limit", "SELECT a FROM t\nLIMIT 5\nORDER BY a, b", "does not precede"},
		{"two top-level orders", "SELECT a FROM t\nORDER BY a, b\nORDER BY a, b\nLIMIT 5", "exactly one"},
		{"top-level union", "SELECT a FROM t\nUNION ALL\nSELECT a FROM u\nORDER BY a, b\nLIMIT 5", "top-level UNION"},
		{"wrong key", "SELECT a FROM t\nORDER BY a\nLIMIT 5", "want \"a, b\""},
		{"order only in a string", "SELECT 'ORDER BY a, b' FROM t\nLIMIT 5", "exactly one"},
		{"order only in a comment", "SELECT a FROM t -- ORDER BY a, b\nLIMIT 5", "exactly one"},
	} {
		err := checkTotalTopLevelOrder(tt.statement, "a, b")
		if err == nil {
			t.Errorf("%s: statement was accepted: %q", tt.name, tt.statement)
		} else if !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want it to say %q", tt.name, err, tt.wantErr)
		}
	}
}

// missingEntries lists the functions that build a LIMIT statement but have no
// pinned order here; staleEntries is the reverse.
func missingEntries(found []string, reads map[string][]orderedLimitRead) []string {
	var out []string
	for _, name := range found {
		if _, covered := reads[name]; !covered {
			out = append(out, name)
		}
	}
	return out
}

func staleEntries(found []string, reads map[string][]orderedLimitRead) []string {
	known := map[string]bool{}
	for _, name := range found {
		known[name] = true
	}
	var out []string
	for name := range reads {
		if !known[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// literalLimitSites returns the functions that put a LIMIT keyword in a string
// literal. WithRowLimit is the one place allowed to; a reader that writes its
// own LIMIT would bypass both the function scan and this order guard.
func literalLimitSites(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("parse readers source: %w", err)
	}
	pkg, ok := pkgs["readers"]
	if !ok {
		return nil, fmt.Errorf("package readers not found in the source directory; found %d packages", len(pkgs))
	}
	found := map[string]bool{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil || fn.Name.Name == "WithRowLimit" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, isLit := n.(*ast.BasicLit)
				if isLit && limitKeyword.MatchString(lit.Value) {
					found[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

var limitKeyword = regexp.MustCompile(`(?i)\bLIMIT\b`)

func TestNoReaderWritesItsOwnLimitLiteral(t *testing.T) {
	t.Parallel()
	got, err := literalLimitSites(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("functions with a LIMIT string literal outside WithRowLimit: %q; use WithRowLimit so the order guard sees the statement", got)
	}
	dir := t.TempDir()
	planted := "package readers\n\nfunc ReadSneaky() string {\n\treturn \"SELECT 1 LIMIT 5\"\n}\n\nfunc WithRowLimit() string { return \"LIMIT \" }\n\nfunc Quiet() string { return \"limits\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "sneaky.go"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sneaky_test.go"), []byte("package readers\n\nfunc InTest() string { return \"LIMIT 1\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted2, err := literalLimitSites(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(planted2, ",") != "ReadSneaky" {
		t.Fatalf("planted scan = %q, want only ReadSneaky", planted2)
	}
}

func TestLimitBuilderScanSeesAPlantedUnlistedReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := "package readers\n\nfunc ReadPlanted() string {\n\treturn WithRowLimit(\"SELECT 1\", 5)\n}\n\nfunc Other() int { return 1 }\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "planted_test.go"), []byte("package readers\n\nfunc InTest() string { return WithRowLimit(\"x\", 1) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := scanLimitBuilders(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(found, ",") != "ReadPlanted" {
		t.Fatalf("found = %q, want only ReadPlanted (test files and non-builders are not readers)", found)
	}
	emptyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyDir, "none.go"), []byte("package readers\n\nfunc Other() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanLimitBuilders(emptyDir); err == nil {
		t.Fatal("a source scan that found no builder must fail, not pass with nothing measured")
	}
	if got := missingEntries(found, map[string][]orderedLimitRead{}); strings.Join(got, ",") != "ReadPlanted" {
		t.Fatalf("missingEntries = %q, want ReadPlanted", got)
	}
	if got := staleEntries(found, map[string][]orderedLimitRead{"ReadGone": nil}); strings.Join(got, ",") != "ReadGone" {
		t.Fatalf("staleEntries = %q, want ReadGone", got)
	}
}

func TestEveryLimitStatementHasATotalTopLevelOrder(t *testing.T) {
	t.Parallel()
	reads := orderedLimitReads(t)

	found := functionsThatBuildALimit(t)
	for _, name := range missingEntries(found, reads) {
		t.Errorf("%s calls WithRowLimit but has no entry in orderedLimitReads: add it with the ORDER BY it must carry", name)
	}
	for _, name := range staleEntries(found, reads) {
		t.Errorf("orderedLimitReads names %s, which no longer calls WithRowLimit", name)
	}

	for builder, entries := range reads {
		for _, entry := range entries {
			entry := entry
			t.Run(builder+"/"+entry.label, func(t *testing.T) {
				client := &fakeClient{}
				if err := entry.run(client); err != nil {
					t.Fatalf("read failed: %v", err)
				}
				if len(client.queries) != 1 {
					t.Fatalf("captured %d statements, want 1", len(client.queries))
				}
				if err := checkTotalTopLevelOrder(client.queries[0].statement, entry.wantOrder); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
