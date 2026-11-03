// Package repokey is the one definition of the canonical repository key
// every consumer of this library validates: a lowercase path of 2-20
// segments in at most 255 bytes (a GitLab path nests 20 groups deep), with
// "*" as the wildcard of a grant. It is a copy of the rule in
// dev-health-ops internal/auth/repokey (an internal package, so it cannot be
// imported); repokey_test.go pins the ops pattern so the two cannot drift.
package repokey

import "regexp"

// MaxSegments and MaxBytes bound a whole repository key.
const (
	MaxSegments = 20
	MaxBytes    = 255
)

// SlugPattern is the pattern of a repository path without the wildcard, as
// an RE2 pattern (ClickHouse match() and Go regexp both read it). Every
// segment but the last is a group name: 1-100 characters, an alphanumeric
// first and last character, no leading '.'. The last is a repository name,
// which may start with '.' (.github), so '.' and '..' stay out. The group
// repeat is unrolled because RE2 refuses a bounded repeat nested in a repeat
// past 1000 copies.
var SlugPattern = slugPattern()

// SchemaPattern is SlugPattern or the wildcard "*".
var SchemaPattern = `^(?:` + body() + `|\*)$`

var (
	slugRE   = regexp.MustCompile(SlugPattern)
	schemaRE = regexp.MustCompile(SchemaPattern)
)

func body() string {
	const group = `[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9])?`
	const last = `(?:[a-z0-9]|[a-z0-9.][a-z0-9._-]{0,98}[a-z0-9])`
	nest := ""
	for i := 0; i < MaxSegments-2; i++ {
		nest = "(?:/" + group + nest + ")?"
	}
	return group + nest + `/` + last
}

func slugPattern() string { return `^` + body() + `$` }

// ValidSlug reports whether key is a lowercase path of 2-20 segments in at
// most 255 bytes.
func ValidSlug(key string) bool { return len(key) <= MaxBytes && slugRE.MatchString(key) }

// Valid reports whether key is ValidSlug or "*".
func Valid(key string) bool { return len(key) <= MaxBytes && schemaRE.MatchString(key) }
