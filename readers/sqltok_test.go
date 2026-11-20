package readers_test

import (
	"fmt"
	"strings"
	"testing"
)

// A small ClickHouse SQL tokenizer for the work-unit read-scope census. It
// knows only lexical rules (strings, quoted identifiers, comments, numbers,
// {name:Type} parameters, operators) and splits a statement into one scope
// per SELECT. Anything it does not know is an error, never a silent skip.

type sqlTokKind int

const (
	tokIdent sqlTokKind = iota
	tokString
	tokNumber
	tokParam
	tokPunct
	tokChild
)

type sqlTok struct {
	kind sqlTokKind
	// text is the identifier name with its quotes removed (ClickHouse reads
	// `a` and "a" as a), the decoded value of a string, or the raw text.
	text       string
	quoted     bool
	start, end int
	child      *sqlQuery
}

func (t sqlTok) isKeyword(kw string) bool {
	return t.kind == tokIdent && !t.quoted && strings.EqualFold(t.text, kw)
}

func (t sqlTok) isPunct(p string) bool {
	return t.kind == tokPunct && t.text == p
}

func (t sqlTok) sameAs(o sqlTok) bool {
	return t.kind == o.kind && t.text == o.text && t.kind != tokChild
}

var sqlMultiPunct = []string{"!=", "<>", "<=", ">=", "==", "||", "->", "::"}

const sqlSinglePunct = "()[],.;+-*/%=<>?:!^&|~@"

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func sqlTokens(src string) ([]sqlTok, error) {
	var out []sqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '\'' || c == '`' || c == '"':
			value, end, err := sqlQuoted(src, i)
			if err != nil {
				return nil, err
			}
			kind := tokString
			if c != '\'' {
				kind = tokIdent
			}
			out = append(out, sqlTok{kind: kind, text: value, quoted: c != '\'', start: i, end: end})
			i = end
		case strings.HasPrefix(src[i:], "--"):
			i = sqlLineEnd(src, i)
		case c == '#':
			// ClickHouse: "# " and "#!" start a line comment; a bare '#' is not
			// valid SQL, so it is refused rather than guessed.
			if i+1 < len(src) && src[i+1] != ' ' && src[i+1] != '!' {
				return nil, fmt.Errorf("byte %d: '#' not followed by a space or '!'", i)
			}
			i = sqlLineEnd(src, i)
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("byte %d: unterminated block comment", i)
			}
			// Whether ClickHouse nests block comments decides what follows the
			// first "*/"; the census refuses the form instead of picking one.
			if strings.Contains(src[i+2:i+2+end], "/*") {
				return nil, fmt.Errorf("byte %d: nested block comment", i)
			}
			i += 2 + end + 2
		case c == '{':
			end := strings.IndexByte(src[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("byte %d: unterminated query parameter", i)
			}
			out = append(out, sqlTok{kind: tokParam, text: src[i : i+end+1], start: i, end: i + end + 1})
			i += end + 1
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(src) && (isIdentPart(src[j]) || src[j] == '.' || ((src[j] == '+' || src[j] == '-') && (src[j-1] == 'e' || src[j-1] == 'E'))) {
				j++
			}
			out = append(out, sqlTok{kind: tokNumber, text: src[i:j], start: i, end: j})
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, sqlTok{kind: tokIdent, text: src[i:j], start: i, end: j})
			i = j
		default:
			matched := ""
			for _, p := range sqlMultiPunct {
				if strings.HasPrefix(src[i:], p) {
					matched = p
					break
				}
			}
			if matched == "" && strings.IndexByte(sqlSinglePunct, c) >= 0 {
				matched = string(c)
			}
			if matched == "" {
				return nil, fmt.Errorf("byte %d: unknown character %q", i, c)
			}
			out = append(out, sqlTok{kind: tokPunct, text: matched, start: i, end: i + len(matched)})
			i += len(matched)
		}
	}
	return out, nil
}

func sqlLineEnd(src string, i int) int {
	if end := strings.IndexByte(src[i:], '\n'); end >= 0 {
		return i + end + 1
	}
	return len(src)
}

// sqlQuoted reads a quoted string or identifier at src[i]: a backslash escapes
// the next byte and a doubled quote is one quote.
func sqlQuoted(src string, i int) (string, int, error) {
	q := src[i]
	var b strings.Builder
	j := i + 1
	for j < len(src) {
		switch src[j] {
		case '\\':
			if j+1 >= len(src) {
				return "", 0, fmt.Errorf("byte %d: unterminated %c", i, q)
			}
			b.WriteByte(src[j+1])
			j += 2
		case q:
			if j+1 < len(src) && src[j+1] == q {
				b.WriteByte(q)
				j += 2
				continue
			}
			return b.String(), j + 1, nil
		default:
			b.WriteByte(src[j])
			j++
		}
	}
	return "", 0, fmt.Errorf("byte %d: unterminated %c", i, q)
}

// sqlQuery is one query: its SELECT scopes (UNION / INTERSECT / EXCEPT
// siblings). A scope holds its own tokens only; a parenthesized child query is
// one tokChild token.
type sqlQuery struct {
	scopes []*sqlScope
}

type sqlScope struct {
	toks []sqlTok
}

func parseSQL(src string) (*sqlQuery, []sqlTok, error) {
	toks, err := sqlTokens(src)
	if err != nil {
		return nil, nil, err
	}
	q, err := parseSQLQuery(toks)
	return q, toks, err
}

func parseSQLQuery(toks []sqlTok) (*sqlQuery, error) {
	var flat []sqlTok
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.isPunct("(") && i+1 < len(toks) && (toks[i+1].isKeyword("SELECT") || toks[i+1].isKeyword("WITH")) {
			j, err := sqlMatchParen(toks, i)
			if err != nil {
				return nil, err
			}
			child, err := parseSQLQuery(toks[i+1 : j])
			if err != nil {
				return nil, err
			}
			flat = append(flat, sqlTok{kind: tokChild, text: "(subquery)", start: t.start, end: toks[j].end, child: child})
			i = j
			continue
		}
		flat = append(flat, t)
	}
	q := &sqlQuery{}
	depth, from := 0, 0
	for i, t := range flat {
		switch {
		case t.isPunct("(") || t.isPunct("["):
			depth++
		case t.isPunct(")") || t.isPunct("]"):
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("byte %d: unbalanced %s", t.start, t.text)
			}
		case depth == 0 && (t.isKeyword("UNION") || t.isKeyword("INTERSECT") || t.isKeyword("EXCEPT")):
			q.scopes = append(q.scopes, &sqlScope{toks: flat[from:i]})
			from = i + 1
			if from < len(flat) && (flat[from].isKeyword("ALL") || flat[from].isKeyword("DISTINCT")) {
				from++
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses")
	}
	q.scopes = append(q.scopes, &sqlScope{toks: flat[from:]})
	return q, nil
}

func sqlMatchParen(toks []sqlTok, open int) (int, error) {
	depth := 0
	for j := open; j < len(toks); j++ {
		switch {
		case toks[j].isPunct("("):
			depth++
		case toks[j].isPunct(")"):
			depth--
			if depth == 0 {
				return j, nil
			}
		}
	}
	return 0, fmt.Errorf("byte %d: unbalanced (", toks[open].start)
}

// allScopes returns every scope of q, children included, parents first.
func (q *sqlQuery) allScopes() []*sqlScope {
	var out []*sqlScope
	for _, s := range q.scopes {
		out = append(out, s)
		for _, t := range s.toks {
			if t.kind == tokChild {
				out = append(out, t.child.allScopes()...)
			}
		}
	}
	return out
}

func sqlTokText(toks []sqlTok) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}

func TestSQLTokenizer(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"doubled quote", `SELECT 'it''s' AS x`, "SELECT it's AS x"},
		{"backslash quote", `SELECT 'it\'s -- no' AS x`, "SELECT it's -- no AS x"},
		{"dash dash in string", `SELECT '--' AS sep, a`, "SELECT -- AS sep , a"},
		{"block opener in string", `SELECT '/*' AS o, a, '*/' AS c`, "SELECT /* AS o , a , */ AS c"},
		{"backtick ident", "SELECT `from_ts` FROM `db`.`t`", "SELECT from_ts FROM db . t"},
		{"double-quote ident", `SELECT "a""b" FROM t`, `SELECT a"b FROM t`},
		{"line comments", "SELECT a -- x\n, b # y\n, c #! z\nFROM t", "SELECT a , b , c FROM t"},
		{"comment at EOF", "SELECT a -- x", "SELECT a"},
		{"hash comment at EOF", "SELECT a # x", "SELECT a"},
		{"block comment", "SELECT a /* x, y */ , b", "SELECT a , b"},
		{"param", "WHERE org_id = {org_id:String}", "WHERE org_id = {org_id:String}"},
		{"numbers", "x[3] + 1.5e-3, t.1", "x [ 3 ] + 1.5e-3 , t . 1"},
	}
	for _, c := range cases {
		toks, err := sqlTokens(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := sqlTokText(toks); got != c.want {
			t.Errorf("%s: tokens %q, want %q", c.name, got, c.want)
		}
	}
	for name, src := range map[string]string{
		"unterminated string":  "SELECT 'abc",
		"unterminated ident":   "SELECT `abc",
		"unterminated comment": "SELECT a /* x",
		"nested comment":       "SELECT a /* x /* y */ z */",
		"heredoc":              "SELECT $$x$$",
		"bare hash":            "SELECT a #x",
		"unterminated param":   "WHERE a = {p:String",
	} {
		if _, err := sqlTokens(src); err == nil {
			t.Errorf("%s: %q tokenized, want an error", name, src)
		}
	}
}

func TestSQLScopes(t *testing.T) {
	q, _, err := parseSQL(`SELECT a FROM (WITH x AS (SELECT b FROM t1) SELECT c, (SELECT d FROM t2) AS e FROM x UNION ALL SELECT f FROM t3) LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range q.allScopes() {
		got = append(got, sqlTokText(s.toks))
	}
	want := []string{
		"SELECT a FROM (subquery) LIMIT 1",
		"WITH x AS (subquery) SELECT c , (subquery) AS e FROM x",
		"SELECT b FROM t1",
		"SELECT d FROM t2",
		"SELECT f FROM t3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("scopes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
