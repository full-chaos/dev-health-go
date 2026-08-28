package authverify

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveRequestedScope_emptyRequestReturnsFullGrant(t *testing.T) {
	binding := WorkloadBinding{GrantedScopes: []string{"scope:read", "scope:write"}}
	scope, err := ResolveRequestedScope(binding, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope, []string{"scope:read", "scope:write"}) {
		t.Fatalf("scope = %#v", scope)
	}
}

func TestResolveRequestedScope_narrowsToRequestedSubset(t *testing.T) {
	binding := WorkloadBinding{GrantedScopes: []string{"scope:read", "scope:write"}}
	scope, err := ResolveRequestedScope(binding, []string{"scope:read"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope, []string{"scope:read"}) {
		t.Fatalf("scope = %#v", scope)
	}
}

func TestResolveRequestedScope_rejectsWideningBeyondTheGrant(t *testing.T) {
	binding := WorkloadBinding{GrantedScopes: []string{"scope:read"}}
	if _, err := ResolveRequestedScope(binding, []string{"scope:write"}); !errors.Is(err, ErrScopeNotGranted) {
		t.Fatalf("error = %v, want ErrScopeNotGranted", err)
	}
}

func TestResolveRequestedScope_dedupesRequestedScopes(t *testing.T) {
	binding := WorkloadBinding{GrantedScopes: []string{"scope:read", "scope:write"}}
	scope, err := ResolveRequestedScope(binding, []string{"scope:read", "scope:read"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scope, []string{"scope:read"}) {
		t.Fatalf("scope = %#v, want deduped", scope)
	}
}

func TestResolveRequestedScope_emptyGrantedScopesIsWorkloadBindingNotFound(t *testing.T) {
	if _, err := ResolveRequestedScope(WorkloadBinding{}, nil); !errors.Is(err, ErrWorkloadBindingNotFound) {
		t.Fatalf("error = %v, want ErrWorkloadBindingNotFound", err)
	}
}
