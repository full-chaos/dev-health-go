package readers_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/latestrow"
	"github.com/full-chaos/dev-health-go/readers"
)

// The read-scope census reads the SQL each registered builder renders (the
// go/ast inventory in work_unit_version_key_census_test.go only places the
// table names). It tokenizes the true render (sqltok_test.go), splits it into
// one scope per SELECT, and requires exactly one scope that reads the table in
// relation position. That scope must EQUAL the builder's allowlist: the select
// items in order, each version pick argMax(<column>, latestrow key) AS
// <column>, and the exact FROM / WHERE / GROUP BY; any other item or clause
// fails. The only extra item allowed is a parenthesized subquery that does not
// name the table (it cannot pick a version). A WHERE subquery fails by design
// (it can filter versions); a builder that needs one changes its allowlist.
// Each pick's bytes must come from a latestrow call: the builder's Go source
// is parsed, its SQL concat is aligned byte for byte with the render, and the
// call is found by import path with constant arguments, so a byte-equal inline
// copy of the key fails.
//
// NOT pinned: SQL assembled outside the registered builders (the inventory
// refuses its table literals), correlated subqueries (an experimental
// ClickHouse setting), heredoc strings and nested block comments (refused by
// the tokenizer), and what ClickHouse does with the SQL (the tie cells).

const latestrowImportPath = "github.com/full-chaos/dev-health-go/latestrow"

type readItem struct {
	column string
	pick   bool
}

type readBuilder struct {
	name, file, table string
	// sqlArgOf names the call whose first argument is the SQL; "" means the
	// decl's single return value.
	sqlArgOf string
	items    []readItem
	rest     string
	suffix   string
	render   func(readers.TimeBound) string
	// plant anchors: each appears once in the Go source and once in the render.
	indent, from, selectHead string
}

func (b readBuilder) siteCalls(column string) []string {
	if b.table == "work_unit_investments" {
		return []string{fmt.Sprintf("ArgMax(%q)", column), fmt.Sprintf("ArgMaxFor(%q,%q)", b.table, column)}
	}
	return []string{fmt.Sprintf("ArgMaxFor(%q,%q)", b.table, column)}
}

func (b readBuilder) siteGo(column string) string {
	if b.table == "work_unit_investments" {
		return "`+latestrow.ArgMax(\"" + column + "\")+`"
	}
	return "` + latestrow.ArgMaxFor(\"" + b.table + "\", \"" + column + "\") + `"
}

func (b readBuilder) siteSQL(column string) string {
	return latestrow.ArgMaxFor(b.table, column)
}

func (b readBuilder) picks() []string {
	var out []string
	for _, it := range b.items {
		if it.pick {
			out = append(out, it.column)
		}
	}
	return out
}

func captureStatement(read func(*fakeClient)) string {
	c := &fakeClient{}
	read(c)
	if len(c.queries) != 1 {
		return ""
	}
	return c.queries[0].statement
}

func themePicks(withOrg bool) []readItem {
	items := []readItem{{column: "work_unit_id"}}
	for _, c := range []string{"from_ts", "to_ts", "theme_distribution_json", "subcategory_distribution_json", "structural_evidence_json"} {
		items = append(items, readItem{column: c, pick: true})
	}
	if withOrg {
		items = append(items, readItem{column: "org_id"})
	}
	return items
}

var readScopeBuilders = []readBuilder{
	{
		name: "ReadTeamThemeMix", file: "readers/investment_theme.go", table: "work_unit_investments", sqlArgOf: "WithRowLimit",
		items:  themePicks(true),
		rest:   "FROM work_unit_investments WHERE org_id = {org_id:String} GROUP BY org_id, work_unit_id",
		suffix: "\nLIMIT " + strconv.Itoa(readers.DefaultRowLimit),
		render: func(tb readers.TimeBound) string {
			return captureStatement(func(c *fakeClient) {
				_, _ = readers.ReadTeamThemeMix(context.Background(), c, "org-census", []string{"team-census"}, tb)
			})
		},
		indent: "    ", from: "FROM work_unit_investments\n    WHERE org_id = {org_id:String}\n    GROUP BY org_id, work_unit_id\n", selectHead: "latest AS (\n    SELECT",
	},
	{
		name: "ReadProjectThemeMixWithRowLimit", file: "readers/investment_project_theme.go", table: "work_unit_investments", sqlArgOf: "WithRowLimit",
		items:  themePicks(false),
		rest:   "FROM work_unit_investments WHERE org_id = {org_id:String} GROUP BY work_unit_id",
		suffix: "\nLIMIT 7",
		render: func(tb readers.TimeBound) string {
			return captureStatement(func(c *fakeClient) {
				_, _ = readers.ReadProjectThemeMixWithRowLimit(context.Background(), c, "org-census", []string{"linear:p-census"}, tb, 7)
			})
		},
		indent: "    ", from: "FROM work_unit_investments\n    WHERE org_id = {org_id:String}\n    GROUP BY work_unit_id\n", selectHead: "latest AS (\n    SELECT",
	},
	{
		name: "LatestWorkUnitIssueSizeSource", file: "readers/investment_issue_size.go", table: "work_unit_issue_size",
		items:  []readItem{{column: "org_id"}, {column: "work_unit_id"}, {column: "issue_size", pick: true}},
		rest:   "FROM work_unit_issue_size WHERE org_id = {org_id:String} GROUP BY org_id, work_unit_id",
		render: func(readers.TimeBound) string { return readers.LatestWorkUnitIssueSizeSource() },
		indent: "                ", from: "FROM work_unit_issue_size\n                WHERE org_id = {org_id:String}\n                GROUP BY org_id, work_unit_id\n", selectHead: "(\n                SELECT",
	},
}

var censusBounds = map[string]readers.TimeBound{
	"none":  {},
	"asof":  {Active: true, End: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
	"range": {Active: true, HasStart: true, Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
}

// lrSpan is a byte range of the render that a latestrow call produced.
type lrSpan struct {
	start, end int
	call       string
}

type sqlSegment struct {
	text  string
	known bool
	call  string
}

// alignBuilder parses the builder's Go source, takes its SQL concat operand by
// operand, and aligns it with the render: literal and latestrow operands must
// sit at the cursor byte for byte; any other operand spans to the next known
// one. It returns the byte ranges each latestrow call produced.
func alignBuilder(b readBuilder, goSrc, render string) ([]lrSpan, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, b.file, goSrc, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%s: go parse: %w", b.name, err)
	}
	lrName := ""
	for _, imp := range file.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == latestrowImportPath {
			lrName = "latestrow"
			if imp.Name != nil {
				lrName = imp.Name.Name
			}
		}
	}
	consts := map[string]ast.Expr{}
	collectConsts := func(decl *ast.GenDecl) {
		if decl.Tok != token.CONST {
			return
		}
		for _, spec := range decl.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i < len(vs.Values) {
					consts[n.Name] = vs.Values[i]
				}
			}
		}
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			collectConsts(d)
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == b.name {
				fn = d
			}
		}
	}
	if fn == nil {
		return nil, fmt.Errorf("%s: no func %s in %s", b.name, b.name, b.file)
	}
	var sqlExprs []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeclStmt:
			if g, ok := x.Decl.(*ast.GenDecl); ok {
				collectConsts(g)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && b.sqlArgOf != "" && id.Name == b.sqlArgOf && len(x.Args) > 0 {
				sqlExprs = append(sqlExprs, x.Args[0])
			}
		case *ast.ReturnStmt:
			if b.sqlArgOf == "" && len(x.Results) == 1 {
				sqlExprs = append(sqlExprs, x.Results[0])
			}
		}
		return true
	})
	if len(sqlExprs) != 1 {
		return nil, fmt.Errorf("%s: %d SQL expressions, want 1", b.name, len(sqlExprs))
	}
	var constString func(e ast.Expr, seen int) (string, bool)
	constString = func(e ast.Expr, seen int) (string, bool) {
		if seen > 16 {
			return "", false
		}
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				s, err := strconv.Unquote(x.Value)
				return s, err == nil
			}
		case *ast.Ident:
			if v, ok := consts[x.Name]; ok {
				return constString(v, seen+1)
			}
		case *ast.ParenExpr:
			return constString(x.X, seen+1)
		}
		return "", false
	}
	var operands func(ast.Expr) []ast.Expr
	operands = func(e ast.Expr) []ast.Expr {
		switch x := e.(type) {
		case *ast.ParenExpr:
			return operands(x.X)
		case *ast.BinaryExpr:
			if x.Op == token.ADD {
				return append(operands(x.X), operands(x.Y)...)
			}
		}
		return []ast.Expr{e}
	}
	var segs []sqlSegment
	for _, op := range operands(sqlExprs[0]) {
		if s, ok := constString(op, 0); ok {
			segs = append(segs, sqlSegment{text: s, known: true})
			continue
		}
		if sel, ok := op.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && lrName != "" && id.Name == lrName && sel.Sel.Name == "OrderKey" {
				segs = append(segs, sqlSegment{text: latestrow.OrderKey, known: true, call: "OrderKey"})
				continue
			}
		}
		call, ok := op.(*ast.CallExpr)
		if !ok {
			segs = append(segs, sqlSegment{})
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			segs = append(segs, sqlSegment{})
			continue
		}
		if id, ok := sel.X.(*ast.Ident); !ok || lrName == "" || id.Name != lrName {
			segs = append(segs, sqlSegment{})
			continue
		}
		args := make([]string, len(call.Args))
		quoted := make([]string, len(call.Args))
		for i, a := range call.Args {
			s, ok := constString(a, 0)
			if !ok {
				return nil, fmt.Errorf("%s: latestrow.%s argument %d is not a constant string", b.name, sel.Sel.Name, i+1)
			}
			args[i], quoted[i] = s, strconv.Quote(s)
		}
		var text string
		switch {
		case sel.Sel.Name == "ArgMax" && len(args) == 1:
			text = latestrow.ArgMax(args[0])
		case sel.Sel.Name == "ArgMaxFor" && len(args) == 2:
			text = latestrow.ArgMaxFor(args[0], args[1])
		case sel.Sel.Name == "ArgMaxKeepNull" && len(args) == 1:
			text = latestrow.ArgMaxKeepNull(args[0])
		case sel.Sel.Name == "ArgMaxKeepNullBy" && len(args) == 2:
			text = latestrow.ArgMaxKeepNullBy(args[0], args[1])
		case sel.Sel.Name == "OrderKeyFor" && len(args) == 1:
			text = latestrow.OrderKeyFor(args[0])
		default:
			return nil, fmt.Errorf("%s: unknown latestrow call %s/%d", b.name, sel.Sel.Name, len(args))
		}
		segs = append(segs, sqlSegment{text: text, known: true, call: sel.Sel.Name + "(" + strings.Join(quoted, ",") + ")"})
	}
	body := strings.TrimSuffix(render, b.suffix)
	if body == render && b.suffix != "" {
		return nil, fmt.Errorf("%s: render does not end in %q", b.name, b.suffix)
	}
	var spans []lrSpan
	cursor, open := 0, false
	for _, s := range segs {
		if !s.known {
			open = true
			continue
		}
		if s.text == "" {
			continue
		}
		at := cursor
		if open {
			idx := strings.Index(body[cursor:], s.text)
			if idx < 0 {
				return nil, fmt.Errorf("%s: Go operand %.40q not found in the render after byte %d", b.name, s.text, cursor)
			}
			at = cursor + idx
		} else if !strings.HasPrefix(body[cursor:], s.text) {
			return nil, fmt.Errorf("%s: Go operand %.40q does not align with the render at byte %d", b.name, s.text, cursor)
		}
		if s.call != "" {
			spans = append(spans, lrSpan{start: at, end: at + len(s.text), call: s.call})
		}
		cursor, open = at+len(s.text), false
	}
	if !open && cursor != len(body) {
		return nil, fmt.Errorf("%s: render holds %d bytes after the last Go operand", b.name, len(body)-cursor)
	}
	return spans, nil
}

func scopeNamesTable(q *sqlQuery, table string) bool {
	for _, s := range q.allScopes() {
		for _, t := range s.toks {
			if t.kind == tokIdent && strings.EqualFold(t.text, table) {
				return true
			}
		}
	}
	return false
}

// scopeReads reports whether the scope reads table in relation position (after
// FROM, JOIN, or a top-level comma of the FROM list, bare or db-qualified, or
// as a table-function argument), and returns references to the table it
// cannot classify (neither a relation nor a column qualifier).
func scopeReads(s *sqlScope, table string) (bool, []string) {
	toks := s.toks
	classified := map[int]bool{}
	reads := false
	relationAt := func(p int) {
		if p >= len(toks) || toks[p].kind != tokIdent {
			return
		}
		name := p
		if p+2 < len(toks) && toks[p+1].isPunct(".") && toks[p+2].kind == tokIdent {
			name = p + 2
		}
		if name+1 < len(toks) && toks[name+1].isPunct("(") {
			end, err := sqlMatchParen(toks, name+1)
			if err != nil {
				end = len(toks) - 1
			}
			for k := name + 2; k < end; k++ {
				if (toks[k].kind == tokIdent || toks[k].kind == tokString) && strings.EqualFold(toks[k].text, table) {
					reads, classified[k] = true, true
				}
			}
			return
		}
		if strings.EqualFold(toks[name].text, table) {
			reads, classified[name] = true, true
		}
	}
	depth, inFrom := 0, false
	for i, t := range toks {
		switch {
		case t.isPunct("(") || t.isPunct("["):
			depth++
		case t.isPunct(")") || t.isPunct("]"):
			depth--
		case depth != 0:
		case t.isKeyword("FROM"):
			inFrom = true
			relationAt(i + 1)
		case t.isKeyword("JOIN"):
			relationAt(i + 1)
		case inFrom && t.isPunct(","):
			relationAt(i + 1)
		case t.isKeyword("WHERE") || t.isKeyword("PREWHERE") || t.isKeyword("GROUP") || t.isKeyword("HAVING") ||
			t.isKeyword("ORDER") || t.isKeyword("LIMIT") || t.isKeyword("SETTINGS") || t.isKeyword("WINDOW") ||
			t.isKeyword("QUALIFY") || t.isKeyword("FORMAT"):
			inFrom = false
		}
	}
	var odd []string
	for i, t := range toks {
		if t.kind == tokIdent && strings.EqualFold(t.text, table) && !classified[i] && !(i+1 < len(toks) && toks[i+1].isPunct(".")) {
			odd = append(odd, fmt.Sprintf("byte %d", t.start))
		}
	}
	return reads, odd
}

func splitTopLevel(toks []sqlTok) [][]sqlTok {
	var out [][]sqlTok
	depth, from := 0, 0
	for i, t := range toks {
		switch {
		case t.isPunct("(") || t.isPunct("["):
			depth++
		case t.isPunct(")") || t.isPunct("]"):
			depth--
		case depth == 0 && t.isPunct(","):
			out = append(out, toks[from:i])
			from = i + 1
		}
	}
	return append(out, toks[from:])
}

func stripOuterParens(toks []sqlTok) []sqlTok {
	for len(toks) >= 2 && toks[0].isPunct("(") {
		end, err := sqlMatchParen(toks, 0)
		if err != nil || end != len(toks)-1 {
			break
		}
		toks = toks[1 : len(toks)-1]
	}
	return toks
}

func sameTokens(a, b []sqlTok) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].sameAs(b[i]) {
			return false
		}
	}
	return true
}

func mustTokens(src string) []sqlTok {
	toks, err := sqlTokens(src)
	if err != nil {
		panic(err)
	}
	return toks
}

// checkReadScope compares the one read scope with the builder's allowlist.
func checkReadScope(b readBuilder, s *sqlScope, render string, spans []lrSpan) []string {
	var errs []string
	toks := s.toks
	text := func(ts []sqlTok) string {
		if len(ts) == 0 {
			return ""
		}
		return strings.Join(strings.Fields(render[ts[0].start:ts[len(ts)-1].end]), " ")
	}
	if len(toks) == 0 || !toks[0].isKeyword("SELECT") {
		return []string{fmt.Sprintf("%s: the read scope does not start with SELECT: %q", b.name, text(toks))}
	}
	from := -1
	depth := 0
	for i, t := range toks {
		if t.isPunct("(") || t.isPunct("[") {
			depth++
		} else if t.isPunct(")") || t.isPunct("]") {
			depth--
		} else if depth == 0 && t.isKeyword("FROM") {
			from = i
			break
		}
	}
	if from < 0 {
		return []string{fmt.Sprintf("%s: the read scope has no FROM", b.name)}
	}
	if !sameTokens(toks[from:], mustTokens(b.rest)) {
		errs = append(errs, fmt.Sprintf("%s: the read scope's clauses are %q, want exactly %q", b.name, text(toks[from:]), b.rest))
	}
	allowed := map[string]bool{}
	for _, it := range b.items {
		allowed[it.column] = true
	}
	j := 0
	for _, item := range splitTopLevel(toks[1:from]) {
		if len(item) == 0 {
			errs = append(errs, fmt.Sprintf("%s: empty select item", b.name))
			continue
		}
		if item[0].kind == tokChild && !scopeNamesTable(item[0].child, b.table) &&
			(len(item) == 1 || (len(item) == 3 && item[1].isKeyword("AS") && item[2].kind == tokIdent && !allowed[item[2].text])) {
			continue
		}
		if j >= len(b.items) {
			errs = append(errs, fmt.Sprintf("%s: extra select item %q", b.name, text(item)))
			continue
		}
		want := b.items[j]
		j++
		if !want.pick {
			if len(item) != 1 || item[0].kind != tokIdent || item[0].text != want.column {
				errs = append(errs, fmt.Sprintf("%s: select item %q, want %s", b.name, text(item), want.column))
			}
			continue
		}
		site := b.name + "/" + want.column
		n := len(item)
		if n < 3 || !item[n-2].isKeyword("AS") || item[n-1].kind != tokIdent {
			errs = append(errs, fmt.Sprintf("%s: pick %q has no AS alias", site, text(item)))
			continue
		}
		if item[n-1].text != want.column {
			errs = append(errs, fmt.Sprintf("%s: pick is aliased %q, want %q", site, item[n-1].text, want.column))
		}
		expr := stripOuterParens(item[:n-2])
		if !sameTokens(expr, mustTokens("argMax("+want.column+", "+latestrow.OrderKeyFor(b.table)+")")) {
			errs = append(errs, fmt.Sprintf("%s: pick %q is not argMax(%s, latestrow key)", site, text(expr), want.column))
			continue
		}
		start, end := expr[0].start, expr[len(expr)-1].end
		fromLatestrow := false
		for _, sp := range spans {
			for _, call := range b.siteCalls(want.column) {
				if sp.call == call && sp.start <= start && end <= sp.end {
					fromLatestrow = true
				}
			}
		}
		if !fromLatestrow {
			errs = append(errs, fmt.Sprintf("%s: the pick's bytes do not come from latestrow.%s (an inline copy of the key)", site, b.siteCalls(want.column)[0]))
		}
	}
	if j < len(b.items) {
		errs = append(errs, fmt.Sprintf("%s: %d select items match the allowlist slots, want %d (missing %s)", b.name, j, len(b.items), b.items[j].column))
	}
	return errs
}

// checkBuilder runs the census on one builder's Go source and render.
func checkBuilder(b readBuilder, goSrc, render string) []string {
	spans, err := alignBuilder(b, goSrc, render)
	if err != nil {
		return []string{err.Error()}
	}
	q, _, err := parseSQL(render)
	if err != nil {
		return []string{fmt.Sprintf("%s: tokenizer: %v", b.name, err)}
	}
	var errs []string
	var readScopes []*sqlScope
	for _, s := range q.allScopes() {
		reads, odd := scopeReads(s, b.table)
		if reads {
			readScopes = append(readScopes, s)
		}
		for _, o := range odd {
			errs = append(errs, fmt.Sprintf("%s: %s names %s outside a relation or column qualifier", b.name, o, b.table))
		}
	}
	if len(readScopes) != 1 {
		return append(errs, fmt.Sprintf("%s: %d scopes read %s, want 1", b.name, len(readScopes), b.table))
	}
	return append(errs, checkReadScope(b, readScopes[0], render, spans)...)
}

func builderSource(t *testing.T, b readBuilder) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", b.file))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

func TestWorkUnitVersionReadScopeBuildersAreTheRegisteredOnes(t *testing.T) {
	specs := map[string]bool{}
	for _, b := range readScopeBuilders {
		specs[b.table+"|"+b.file+":"+b.name] = true
	}
	for _, decl := range workUnitRegisteredDecls {
		if strings.Contains(decl, "|readers/") && !specs[decl] {
			t.Errorf("registered reader %s has no read-scope allowlist in readScopeBuilders", decl)
		}
		delete(specs, decl)
	}
	for spec := range specs {
		t.Errorf("read-scope allowlist %s is not a registered declaration", spec)
	}
}

func TestWorkUnitVersionReadScopeCensus(t *testing.T) {
	for _, b := range readScopeBuilders {
		src := builderSource(t, b)
		for name, tb := range censusBounds {
			render := b.render(tb)
			if render == "" {
				t.Fatalf("%s/%s: no statement rendered", b.name, name)
			}
			for _, e := range checkBuilder(b, src, render) {
				t.Errorf("%s: %s", name, e)
			}
		}
	}
}

type censusEdit struct {
	old, new string
	all      bool
}

type censusPlant struct {
	name    string
	goEdits []censusEdit
	sqlEdit []censusEdit
	// red: the census must fail; site, when set, must be named and no other
	// site of the builder may be.
	red  bool
	site string
}

func applyEdits(t *testing.T, what, src string, edits []censusEdit) string {
	t.Helper()
	for _, e := range edits {
		if e.old == "" {
			src += e.new
			continue
		}
		n := strings.Count(src, e.old)
		if n == 0 || (!e.all && n != 1) {
			t.Fatalf("%s: anchor %q found %d times", what, e.old, n)
		}
		src = strings.ReplaceAll(src, e.old, e.new)
	}
	return src
}

func both(old, new string) ([]censusEdit, []censusEdit) {
	e := []censusEdit{{old: old, new: new}}
	return e, e
}

func censusPlants(b readBuilder) []censusPlant {
	first := b.picks()[0]
	tq := b.table + "." + first
	var plants []censusPlant
	add := func(name string, red bool, goEdits, sqlEdits []censusEdit) {
		plants = append(plants, censusPlant{name: name, red: red, goEdits: goEdits, sqlEdit: sqlEdits})
	}
	item := func(name string, red bool, extra string) {
		g, s := both("\n"+b.indent+b.from, ",\n"+b.indent+extra+"\n"+b.indent+b.from)
		add(name, red, g, s)
	}
	clause := func(name string, red bool, extra string) {
		g, s := both(b.from, b.from+b.indent+extra+"\n")
		add(name, red, g, s)
	}
	pick := func(name string, red bool, goNew, sqlNew string) {
		add(name, red, []censusEdit{{old: b.siteGo(first), new: goNew}}, []censusEdit{{old: b.siteSQL(first), new: sqlNew}})
	}
	aliasOld := " AS " + first
	alias := func(name string, red bool, newAlias string) {
		// A backtick cannot sit in a Go raw string: the Go side spells it as
		// a quoted operand of the concat.
		goAlias := newAlias
		if strings.Contains(newAlias, "`") {
			goAlias = "`+" + strconv.Quote(newAlias) + "+`"
		}
		end := ","
		if b.table != "work_unit_investments" {
			end = "\n"
		}
		add(name, red, []censusEdit{{old: aliasOld + end, new: " AS " + goAlias + end}}, []censusEdit{{old: aliasOld + end, new: " AS " + newAlias + end}})
	}
	site, siteSQL := b.siteGo(first), b.siteSQL(first)

	// Form 1: an added pick in the read.
	for _, f := range []string{"min(%s)", "max(%s)", "any(%s)", "any_value(%s)", "argMin(%s, computed_at)", "groupArray(%s)[1]", "topK(1)(%s)[1]"} {
		item("form1/"+strings.SplitN(f, "(", 2)[0], true, fmt.Sprintf(f, tq)+" AS extra_pick")
	}
	// Form 2: row-cutting clauses, any spelling.
	for name, c := range map[string]string{
		"order-limit-1": "ORDER BY work_unit_id LIMIT 1",
		"limit-1-by":    "LIMIT 1 BY work_unit_id",
		"limit-by-case": "limit\n1\n  by work_unit_id",
		"limit-paren":   "LIMIT (1) BY work_unit_id",
		"limit-offset":  "LIMIT 0, 1 BY work_unit_id",
		"settings":      "SETTINGS max_threads = 1",
	} {
		clause("form2/"+name, true, c)
	}
	g, s := both(b.selectHead, b.selectHead+" DISTINCT ON (work_unit_id)")
	add("form2/distinct-on", true, g, s)
	g, s = both(b.from[:len("FROM "+b.table)], "FROM "+b.table+" FINAL")
	add("form2/final", true, g, s)
	g, s = both(b.from[:len("FROM "+b.table)], "FROM "+b.table+" AS wui final")
	add("form2/alias-final", true, g, s)
	// D1: a WHERE subquery on another table fails by design.
	g, s = both(b.from, strings.Replace(b.from, "{org_id:String}", "{org_id:String} AND work_unit_id IN (SELECT work_unit_id FROM work_item_team_attributions)", 1))
	add("d1/where-subquery", true, g, s)
	// Form 3: the pick's output alias.
	alias("form3/wrong-alias", true, first+"_diag")
	alias("form3/commented-suffix", true, "/* keep */ "+first+"_diag")
	alias("form3/quoted-wrong", true, `"`+first+`_diag"`)
	alias("form3/quoted-suffix", true, "`"+first+"`_x")
	alias("d2/quoted-right-backtick", false, "`"+first+"`")
	alias("d2/quoted-right-double", false, `"`+first+`"`)
	pick("form3/parenthesized-pick", false, "("+site+")", "("+siteSQL+")")
	pick("form3/double-parenthesized-pick", false, "(("+site+"))", "(("+siteSQL+"))")
	// Form 4: a pick hidden or shown by a comment form.
	pick("form4/dash-comment-hides-pick", true, "-- "+site, "-- "+siteSQL)
	pick("form4/hash-comment-hides-pick", true, "# "+site, "# "+siteSQL)
	pick("form4/shebang-comment-hides-pick", true, "#! "+site, "#! "+siteSQL)
	pick("form4/block-comment-hides-pick", true, "/* "+site+" */", "/* "+siteSQL+" */")
	pick("form4/block-comment-shows-pick", true, "/* "+site+" */ "+first, "/* "+siteSQL+" */ "+first)
	item("form4/dash-in-string", true, "'--' AS sep, any("+tq+") AS any_pick")
	item("form4/block-in-string", true, "'/*' AS open_c, any("+tq+") AS any_pick, '*/' AS close_c")
	g, s = both(b.from, "-- note: a comment\n"+b.indent+"# note: a hash comment\n"+b.indent+"#! note\n"+b.indent+"/* block note */ "+b.from)
	add("form4/comments-outside-strings", false, g, s)
	// Form 5: an other-table subquery inside the read.
	item("form5/other-table-scalar-subquery", false, "(SELECT argMax(team_id, computed_at) FROM work_item_team_attributions) AS other_pick")
	item("form5/same-table-scalar-subquery", true, "(SELECT max(computed_at) FROM "+b.table+") AS other_pick")
	// Form 6: a byte-equal inline copy of the key.
	add("form6/inline-key-copy", true, []censusEdit{{old: site, new: siteSQL}}, nil)
	// Go-side controls and refusals.
	add("go/import-alias", false, []censusEdit{
		{old: `"` + latestrowImportPath + `"`, new: `lr "` + latestrowImportPath + `"`},
		{old: "latestrow.ArgMax", new: "lr.ArgMax", all: true},
	}, nil)
	constGo := strings.Replace(site, `"`+first+`"`, "censusColumn", 1)
	add("go/const-argument", false, []censusEdit{{old: site, new: constGo}, {new: "\nconst censusColumn = \"" + first + "\"\n"}}, nil)
	add("go/multiline-call", false, []censusEdit{{old: site, new: strings.Replace(strings.Replace(site, "(", "(\n\t\t", 1), ")+", ",\n\t)+", 1)}}, nil)
	add("go/non-constant-argument", true, []censusEdit{{old: site, new: strings.Replace(site, `"`+first+`"`, "strings.TrimSpace(\""+first+"\")", 1)}}, nil)
	add("go/other-package-named-latestrow", true, []censusEdit{{old: `"` + latestrowImportPath + `"`, new: `latestrow "` + latestrowImportPath + `copy"`}}, nil)
	if b.table == "work_unit_investments" {
		sibling := "\nrollup AS (\n    SELECT work_unit_id, argMax(effort_value, computed_at) AS effort_value FROM work_unit_investments_rollup GROUP BY work_unit_id\n),\nwindowed AS ("
		g, s = both("\nwindowed AS (", sibling)
		add("form5/related-table-sibling-cte", false, g, s)
		g, s = both("\nwindowed AS (", "\nother AS (\n    SELECT work_item_id, argMax(team_id, computed_at) AS team_id FROM work_item_team_attributions GROUP BY work_item_id\n),\nwindowed AS (")
		add("form5/other-table-sibling-cte", false, g, s)
		g, s = both("\nwindowed AS (", "\nsecond AS (\n    SELECT work_unit_id FROM work_unit_investments\n),\nwindowed AS (")
		add("second-read", true, g, s)
	}
	// Form 7: a partial key of its own at each site.
	for _, c := range b.picks() {
		for name, key := range map[string]string{
			"no-part-offset":  "(" + b.table + ".computed_at, toUInt64OrZero(splitByChar('_', " + b.table + "._part)[3]))",
			"no-block-number": "(" + b.table + ".computed_at, " + b.table + "._part_offset)",
		} {
			expr := "argMax(" + c + ", " + key + ")"
			plants = append(plants, censusPlant{
				name: "form7/" + c + "/" + name, red: true, site: c,
				goEdits: []censusEdit{{old: b.siteGo(c), new: expr}},
				sqlEdit: []censusEdit{{old: b.siteSQL(c), new: expr}},
			})
		}
	}
	return plants
}

func TestWorkUnitVersionReadScopeCensusPlants(t *testing.T) {
	for _, b := range readScopeBuilders {
		src := builderSource(t, b)
		render := b.render(censusBounds["range"])
		for _, p := range censusPlants(b) {
			t.Run(b.name+"/"+p.name, func(t *testing.T) {
				goSrc := applyEdits(t, "go source", src, p.goEdits)
				planted := render
				if p.sqlEdit != nil {
					planted = applyEdits(t, "render", render, p.sqlEdit)
				}
				errs := checkBuilder(b, goSrc, planted)
				for _, e := range errs {
					if strings.HasPrefix(e, b.name+": go parse:") {
						t.Fatalf("the plant is not valid Go: %s", e)
					}
				}
				if !p.red {
					if len(errs) != 0 {
						t.Errorf("control refused:\n%s", strings.Join(errs, "\n"))
					}
					return
				}
				if len(errs) == 0 {
					t.Fatalf("planted form passed the census")
				}
				t.Logf("refused: %s", strings.Join(errs, " | "))
				if p.site == "" {
					return
				}
				named := false
				for _, e := range errs {
					for _, c := range b.picks() {
						if strings.HasPrefix(e, b.name+"/"+c+":") {
							if c != p.site {
								t.Errorf("error names site %s, planted at %s: %s", c, p.site, e)
							}
							named = true
						}
					}
				}
				if !named {
					t.Errorf("no error names site %s/%s:\n%s", b.name, p.site, strings.Join(errs, "\n"))
				}
			})
		}
	}
}
