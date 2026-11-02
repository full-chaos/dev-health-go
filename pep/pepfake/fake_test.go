package pepfake_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/pep"
	"github.com/full-chaos/dev-health-go/pep/pepfake"
)

var _ pep.Decider = (*pepfake.Fake)(nil)

func req() pep.Request {
	return pep.Request{PrincipalReference: "prf_EXAMPLE0000000000000001", Action: "acr.context.read", ResourceType: "repository", ResourceKey: "full-chaos/dev-health-ops", OrganizationID: "org_EXAMPLE0000000000000001", RequestID: "req_EXAMPLE0000000000000000001"}
}

func rule() pepfake.Rule {
	r := req()
	return pepfake.Rule{PrincipalReference: r.PrincipalReference, Action: r.Action, ResourceType: r.ResourceType, ResourceKey: r.ResourceKey, OrganizationID: r.OrganizationID}
}

func TestZeroValueDenies(t *testing.T) {
	var zero pepfake.Fake
	var nilFake *pepfake.Fake
	for name, f := range map[string]*pepfake.Fake{"zero": &zero, "nil": nilFake, "no rules": pepfake.New(), "nil slice": pepfake.New(nilRules()...)} {
		d := f.Decide(context.Background(), req())
		if d.Allowed() || d.Reason() != pep.ReasonNoDecision || !d.Local() {
			t.Errorf("%s: allowed=%v reason=%s", name, d.Allowed(), d.Reason())
		}
		got := f.DecideBatch(context.Background(), []pep.Request{req(), req()})
		if len(got) != 2 || got[0].Allowed() || got[1].Allowed() {
			t.Errorf("%s batch: %+v", name, got)
		}
	}
}

func nilRules() []pepfake.Rule { return nil }

func TestARuleAllowsOnlyItsExactRequest(t *testing.T) {
	f := pepfake.New(rule())
	if d := f.Decide(context.Background(), req()); !d.Allowed() {
		t.Fatalf("the matching request was denied: %s", d.Reason())
	}
	for name, mut := range map[string]func(*pep.Request){
		"principal": func(r *pep.Request) { r.PrincipalReference = "prf_EXAMPLE0000000000000002" },
		"action":    func(r *pep.Request) { r.Action = "acr.context.write" },
		"type":      func(r *pep.Request) { r.ResourceType = "organization" },
		"key":       func(r *pep.Request) { r.ResourceKey = "full-chaos/dev-health-web" },
		"org":       func(r *pep.Request) { r.OrganizationID = "org_EXAMPLE0000000000000002" },
		"no org":    func(r *pep.Request) { r.OrganizationID = "" },
		"star key":  func(r *pep.Request) { r.ResourceKey = "*" },
	} {
		r := req()
		mut(&r)
		if d := f.Decide(context.Background(), r); d.Allowed() {
			t.Errorf("%s: another request was allowed", name)
		}
	}
	other := req()
	other.RequestID = "req_EXAMPLE0000000000000000002"
	if !f.Decide(context.Background(), other).Allowed() {
		t.Error("the request id must not matter")
	}
}

func TestBatchDecidesEachItem(t *testing.T) {
	f := pepfake.New(rule())
	other := req()
	other.ResourceKey = "full-chaos/dev-health-web"
	got := f.DecideBatch(context.Background(), []pep.Request{req(), other, req()})
	if len(got) != 3 || !got[0].Allowed() || got[1].Allowed() || !got[2].Allowed() {
		t.Fatalf("%+v", got)
	}
	if len(f.DecideBatch(context.Background(), nil)) != 0 {
		t.Fatal("an empty batch")
	}
}

func TestIncompleteRulesNeverMatch(t *testing.T) {
	for name, mut := range map[string]func(*pepfake.Rule){
		"no principal": func(r *pepfake.Rule) { r.PrincipalReference = "" },
		"no action":    func(r *pepfake.Rule) { r.Action = "" },
		"no type":      func(r *pepfake.Rule) { r.ResourceType = "" },
		"no key":       func(r *pepfake.Rule) { r.ResourceKey = "" },
	} {
		r := rule()
		mut(&r)
		blank := req()
		switch name {
		case "no principal":
			blank.PrincipalReference = ""
		case "no action":
			blank.Action = ""
		case "no type":
			blank.ResourceType = ""
		case "no key":
			blank.ResourceKey = ""
		}
		f := pepfake.New(r)
		if f.Decide(context.Background(), req()).Allowed() || f.Decide(context.Background(), blank).Allowed() {
			t.Errorf("%s: an incomplete rule acted as a wildcard", name)
		}
	}
}

func TestRuleObligationsReachTheDecision(t *testing.T) {
	r := rule()
	r.TenantScope = "org_EXAMPLE0000000000000001"
	r.RepositoryAllowlist = []string{"full-chaos/dev-health-ops"}
	r.Redaction = []pep.RedactionClass{pep.RedactFreeText}
	r.MinimumAssurance = pep.AssuranceAAL2
	r.StepUp = true
	d := pepfake.New(r).Decide(context.Background(), req())
	o := d.Obligations()
	if !d.Allowed() || !o.TenantAllows("org_EXAMPLE0000000000000001") || !o.RepositoryAllowed("full-chaos/dev-health-ops") || o.RepositoryAllowed("a/b") || !o.Redacts(pep.RedactFreeText) || !o.AssuranceSatisfiedBy(pep.AssuranceAAL2) || o.AssuranceSatisfiedBy(pep.AssuranceAAL1) || !o.StepUpRequired() {
		t.Fatalf("obligations lost: %+v", o)
	}
	r.RepositoryAllowlist = []string{}
	if pepfake.New(r).Decide(context.Background(), req()).Obligations().RepositoryAllowed("full-chaos/dev-health-ops") {
		t.Fatal("an empty allowlist must allow nothing")
	}
	r.MinimumAssurance = "aal9"
	if pepfake.New(r).Decide(context.Background(), req()).Allowed() {
		t.Fatal("a rule with an unknown assurance level was allowed")
	}
}

func TestRulesAreCopiedOnNew(t *testing.T) {
	rules := []pepfake.Rule{rule()}
	f := pepfake.New(rules...)
	rules[0].Action = "acr.context.write"
	if !f.Decide(context.Background(), req()).Allowed() {
		t.Fatal("the fake shares the caller's rule slice")
	}
}

// The census: the exported API of pepfake is exactly this list. A new exported
// name must be reviewed for a way to build an allow-all fake, then added here.
func TestExportedAPICensus(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	fields := map[string][]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					name := d.Name.Name
					if d.Recv != nil {
						name = recv(d.Recv.List[0].Type) + "." + name
					}
					if d.Name.IsExported() && (d.Recv == nil || ast.IsExported(recv(d.Recv.List[0].Type))) {
						got = append(got, name)
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								got = append(got, "type "+s.Name.Name)
								if st, ok := s.Type.(*ast.StructType); ok {
									for _, f := range st.Fields.List {
										for _, n := range f.Names {
											if n.IsExported() {
												fields[s.Name.Name] = append(fields[s.Name.Name], n.Name)
											}
										}
									}
								}
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() {
									got = append(got, "value "+n.Name)
								}
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(got)
	want := []string{"Fake.Decide", "Fake.DecideBatch", "New", "type Fake", "type Rule"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the exported API of pepfake changed:\n got %v\nwant %v", got, want)
	}
	if len(fields["Fake"]) != 0 {
		t.Errorf("Fake has exported fields %v: a caller could set an allow-all flag", fields["Fake"])
	}
	wantRule := []string{"Action", "MinimumAssurance", "OrganizationID", "PrincipalReference", "Redaction", "RepositoryAllowlist", "ResourceKey", "ResourceType", "StepUp", "TenantScope"}
	sort.Strings(fields["Rule"])
	if !reflect.DeepEqual(fields["Rule"], wantRule) {
		t.Errorf("Rule fields %v, want %v", fields["Rule"], wantRule)
	}
	for _, f := range wantRule {
		if l := strings.ToLower(f); strings.Contains(l, "all") && f != "RepositoryAllowlist" || strings.Contains(l, "default") || strings.Contains(l, "wildcard") {
			t.Errorf("Rule field %s reads like an allow-all switch", f)
		}
	}
}

func recv(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recv(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}

func TestRulesAreDeepCopiedOnNew(t *testing.T) {
	r := rule()
	r.RepositoryAllowlist = []string{"full-chaos/dev-health-ops"}
	r.Redaction = []pep.RedactionClass{pep.RedactFreeText}
	f := pepfake.New(r)
	r.RepositoryAllowlist[0] = "evil/repo"
	r.Redaction[0] = pep.RedactContactDetails
	o := f.Decide(context.Background(), req()).Obligations()
	if !o.RepositoryAllowed("full-chaos/dev-health-ops") || o.RepositoryAllowed("evil/repo") {
		t.Fatal("the fake shares a rule's allowlist with the caller")
	}
	if !o.Redacts(pep.RedactFreeText) || o.Redacts(pep.RedactContactDetails) {
		t.Fatal("the fake shares a rule's redaction list with the caller")
	}
}
