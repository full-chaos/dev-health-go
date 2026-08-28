package readers

import (
	"fmt"
	"time"
)

// TimeBound is a resolved valid-time window for one bounded ClickHouse
// read. The zero value is inactive: no predicate is added and a statement
// keeps its unbounded "current" behavior.
//
// Extracted from acr devhealthfacts's factTimeBound (CHAOS-3781/CHAOS-4377):
// this carries only the neutral SQL-predicate-building half. The
// Fact/grain-specific half (retention-state classification, effective-grain
// composition, axis resolution against a GraphQL-shaped query) stays in
// acr, wrapping this type.
type TimeBound struct {
	Active bool
	// HasStart distinguishes a range (both ends) from a point-in-time bound
	// (upper bound only). A point-in-time bound must NOT gain a lower
	// bound: "the state as of T" means the latest row at or before T,
	// however old that row is.
	HasStart bool
	Start    time.Time
	End      time.Time
}

const (
	BoundStartParam = "time_start"
	BoundEndParam   = "time_end"
)

// DayPredicate bounds a DATE-grained column (a rollup table's `day`). The
// bound values are timestamps, narrowed with toDate to compare against a
// Date column at that column's own grain.
func (b TimeBound) DayPredicate(column string) string {
	if !b.Active {
		return ""
	}
	predicate := fmt.Sprintf(" AND %s <= toDate({%s:DateTime64(6,'UTC')})", column, BoundEndParam)
	if b.HasStart {
		predicate += fmt.Sprintf(" AND %s >= toDate({%s:DateTime64(6,'UTC')})", column, BoundStartParam)
	}
	return predicate
}

// TimestampPredicate bounds a DateTime64 column (computed_at, created_at,
// submitted_at, and similar).
func (b TimeBound) TimestampPredicate(column string) string {
	if !b.Active {
		return ""
	}
	predicate := fmt.Sprintf(" AND %s <= {%s:DateTime64(6,'UTC')}", column, BoundEndParam)
	if b.HasStart {
		predicate += fmt.Sprintf(" AND %s >= {%s:DateTime64(6,'UTC')}", column, BoundStartParam)
	}
	return predicate
}

// ExistencePredicate bounds an entity's own START column -- "did this
// exist yet at the requested time". It applies ONLY the upper bound, even
// for a range: an entity created before a requested window still existed
// during it, so bounding its creation below would silently drop long-lived
// subjects a historical question is usually about.
func (b TimeBound) ExistencePredicate(column string) string {
	if !b.Active {
		return ""
	}
	return fmt.Sprintf(" AND %s <= {%s:DateTime64(6,'UTC')}", column, BoundEndParam)
}

// AsOfExpression renders the requested instant for use inside a derived
// expression (e.g. "was it merged at T"). It is always the END of the
// window: a range question about a derived state is answered at the end of
// the range.
func (b TimeBound) AsOfExpression() string {
	return fmt.Sprintf("{%s:DateTime64(6,'UTC')}", BoundEndParam)
}

// Bindings returns the parameters the predicates above reference. Empty
// when inactive, so no unused parameter is ever sent.
func (b TimeBound) Bindings() []Binding {
	if !b.Active {
		return nil
	}
	bindings := []Binding{{Name: BoundEndParam, Value: b.End}}
	if b.HasStart {
		bindings = append(bindings, Binding{Name: BoundStartParam, Value: b.Start})
	}
	return bindings
}
