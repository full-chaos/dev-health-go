package repokey_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/repokey"
)

func segments(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "g"
	}
	return strings.Join(parts, "/")
}

func TestValidSlug(t *testing.T) {
	t.Parallel()
	long100 := strings.Repeat("a", 100)
	for key, want := range map[string]bool{
		"acme/widgets":                   true,
		"acme/team/widgets":              true,
		segments(20):                     true,
		segments(21):                     false,
		"acme":                           false,
		"":                               false,
		"*":                              false,
		"acme/.github":                   true,
		"acme/team/.github":              true,
		"acme/.team/widgets":             false,
		"acme/..":                        false,
		"acme/.":                         false,
		"acme/team./widgets":             false,
		"acme/-team/widgets":             false,
		"Acme/widgets":                   false,
		"acme/not real":                  false,
		"acme//widgets":                  false,
		"acme/widgets/":                  false,
		long100 + "/" + long100:          true,
		long100 + "a/x":                  false,
		"a/" + long100 + "a":             false,
		strings.Repeat("ab/", 85) + "ab": false,
	} {
		if got := repokey.ValidSlug(key); got != want {
			t.Errorf("ValidSlug(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestValidSlugByteBound(t *testing.T) {
	t.Parallel()
	seg := strings.Repeat("a", 100)
	atLimit := seg + "/" + seg + "/" + strings.Repeat("a", 53)
	if len(atLimit) != repokey.MaxBytes || !repokey.ValidSlug(atLimit) {
		t.Fatalf("%d-byte key refused", len(atLimit))
	}
	if over := atLimit + "a"; repokey.ValidSlug(over) {
		t.Fatalf("%d-byte key accepted", len(over))
	}
}

func TestValidAcceptsWildcard(t *testing.T) {
	t.Parallel()
	if !repokey.Valid("*") || !repokey.Valid("a/b/c") || repokey.Valid("a") || repokey.Valid(segments(21)) {
		t.Fatal("Valid does not follow the schema rule")
	}
}

// The ops repokey.SchemaPattern at db895bb95616cfc412771bf3dfde728313ba32e3
// (dev-health-ops internal/auth/repokey). A change there must change this
// pin and the copy together.
const (
	opsSchemaPatternLen    = 887
	opsSchemaPatternSHA256 = "8adc37710ec88537018bfe796b986a450add9f7c8db9725d8092f97ed40d7bd6"
)

func TestSchemaPatternMatchesOps(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(repokey.SchemaPattern))
	if len(repokey.SchemaPattern) != opsSchemaPatternLen || hex.EncodeToString(sum[:]) != opsSchemaPatternSHA256 {
		t.Fatalf("SchemaPattern drifted from the ops pattern: len %d sha256 %x", len(repokey.SchemaPattern), sum)
	}
}

func TestSlugPatternIsSchemaPatternWithoutWildcard(t *testing.T) {
	t.Parallel()
	want := strings.TrimSuffix(strings.TrimPrefix(repokey.SchemaPattern, "^(?:"), `|\*)$`)
	if repokey.SlugPattern != "^"+want+"$" {
		t.Fatal("SlugPattern is not the SchemaPattern body")
	}
}
