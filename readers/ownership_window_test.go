package readers_test

import (
	"strings"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestOwnershipPredicateNeverFiltersWindowByValidFrom(t *testing.T) {
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for name, bound := range map[string]readers.TimeBound{
		"range": {Active: true, HasStart: true, Start: end.AddDate(0, 0, -60), End: end},
		"point": {Active: true, End: end},
	} {
		if got := readers.OwnershipValidityPredicate(bound); strings.Contains(got, "valid_from") {
			t.Errorf("%s bound filters ownership by valid_from: %s", name, got)
		}
	}
}

func TestOwnershipPredicateClauses(t *testing.T) {
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		bound readers.TimeBound
		want  string
	}{
		"range":     {readers.TimeBound{Active: true, HasStart: true, Start: end.AddDate(0, 0, -60), End: end}, " AND (valid_to IS NULL OR valid_to > {time_start:DateTime64(6,'UTC')})"},
		"point":     {readers.TimeBound{Active: true, End: end}, " AND (valid_to IS NULL OR valid_to > {time_end:DateTime64(6,'UTC')})"},
		"unbounded": {readers.TimeBound{}, " AND valid_from <= now64(3) AND valid_to IS NULL"},
	}
	for name, c := range cases {
		if got := readers.OwnershipValidityPredicate(c.bound); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}
