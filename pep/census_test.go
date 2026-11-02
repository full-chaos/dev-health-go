package pep

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// exportedAPI lists every exported top-level declaration and method of the
// non-test files in dir.
func exportedAPI(t *testing.T, dir string) (names []string, returnsDecision []string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					name := d.Name.Name
					if d.Recv != nil && len(d.Recv.List) == 1 {
						name = receiverName(d.Recv.List[0].Type) + "." + name
						if !ast.IsExported(d.Name.Name) || !ast.IsExported(receiverName(d.Recv.List[0].Type)) {
							continue
						}
					} else if !ast.IsExported(name) {
						continue
					}
					names = append(names, name)
					if d.Type.Results != nil {
						for _, r := range d.Type.Results.List {
							if strings.Contains(typeString(r.Type), "Decision") {
								returnsDecision = append(returnsDecision, name)
							}
						}
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								names = append(names, "type "+s.Name.Name)
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() {
									names = append(names, "value "+n.Name)
								}
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(names)
	sort.Strings(returnsDecision)
	return names, returnsDecision
}

func receiverName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return receiverName(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

func typeString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.ArrayType:
		return "[]" + typeString(x.Elt)
	case *ast.StarExpr:
		return "*" + typeString(x.X)
	case *ast.SelectorExpr:
		return typeString(x.X) + "." + x.Sel.Name
	}
	return ""
}

func TestExportedAPICensus(t *testing.T) {
	names, returns := exportedAPI(t, ".")
	want := []string{
		"Client.DecideBatch", "Client.Decide", "Config", "Decider", "Error", "Error.Error", "Error.Reason",
		"LocalReasons", "New", "Request", "WireReasons",
	}
	var api, values []string
	for _, n := range names {
		if strings.HasPrefix(n, "type ") {
			api = append(api, strings.TrimPrefix(n, "type "))
		} else if strings.HasPrefix(n, "value ") {
			values = append(values, strings.TrimPrefix(n, "value "))
		} else {
			api = append(api, n)
		}
	}
	aliases := []string{"Assurance", "Decision", "Obligations", "Reason", "RedactionClass"}
	api = filterOut(api, append(aliases, "Client"))
	if got := sortedStrings(want); !reflect.DeepEqual(api, got) {
		t.Fatalf("the exported API changed; a new exported name needs review that it cannot build an allow:\n got %v\nwant %v", api, got)
	}
	// Only the client's two decision calls return a Decision: no exported way to
	// build an allow.
	if want := []string{"Client.Decide", "Client.DecideBatch"}; !reflect.DeepEqual(returns, want) {
		t.Fatalf("exported functions returning a Decision: %v, want %v", returns, want)
	}
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), "allowall") {
			t.Errorf("exported value %s", v)
		}
	}
}

func filterOut(in, drop []string) []string {
	var out []string
	for _, s := range in {
		keep := true
		for _, d := range drop {
			if s == d || strings.HasPrefix(s, d+".") && !strings.HasPrefix(s, "Client.") {
				keep = false
			}
		}
		if keep {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func TestDecisionHasNoExportedFieldsAndAClosedMethodSet(t *testing.T) {
	typ := reflect.TypeOf(Decision{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).IsExported() {
			t.Errorf("exported field %s: a caller could set an allow", typ.Field(i).Name)
		}
	}
	var methods []string
	for i := 0; i < typ.NumMethod(); i++ {
		methods = append(methods, typ.Method(i).Name)
	}
	want := []string{"Allowed", "DecisionID", "Err", "ExpiresAt", "Local", "Obligations", "OrganizationID", "PolicyRevision", "PrincipalID", "Reason"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("Decision methods %v, want %v", methods, want)
	}
	ot := reflect.TypeOf(Obligations{})
	for i := 0; i < ot.NumField(); i++ {
		if ot.Field(i).IsExported() {
			t.Errorf("exported Obligations field %s", ot.Field(i).Name)
		}
	}
}

func TestClientsSatisfyDecider(t *testing.T) {
	var _ Decider = (*Client)(nil)
}
