package readers

import "context"

// IncidentRow is one row of operational_incidents' status/severity.
type IncidentRow struct {
	ID       string
	Status   string
	Severity string
}

// ReadIncidents reads operational_incidents.normalized_status/
// normalized_severity (falling back to the raw_* columns), the same
// columns and fallback devhealthsource/tables.go's queryIncidents already
// reads. A soft-deleted incident (is_deleted = 1, devhealthsource's one
// confirmed soft-delete signal for this table) yields no row for that
// subject, the same as any other zero-row match, rather than reporting
// stale data.
//
// CHAOS-3781 Tier B: an incident that had not resolved yet at the
// requested time was open then, whatever its status reads today. status IS
// derivable, because started_at/resolved_at are immutable event columns
// the row already carries.
//
// SEVERITY IS NOT, and round-1 F2 caught it being emitted anyway.
// operational_incidents.normalized_severity is revised IN PLACE with no
// recorded history, so the row holds only its current value. An incident
// escalated from low to critical after the requested time would have had
// the critical severity reported as though it were true then -- current
// data under a historical label. On a bounded historical read, severity is
// therefore forced to a constant empty string in SQL rather than genuinely
// selected -- kept in the SELECT list (not dropped) so the scan shape
// stays identical on both axes, one fewer place for the two paths to
// drift. The caller is responsible for treating an empty severity on a
// historical read as "omitted", never as "recorded empty" (see the acr
// devhealthfacts adapter's incidentSeverityOmittedReason for how that
// distinction surfaces in its public result).
func ReadIncidents(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]IncidentRow, error) {
	statusExpression := "ifNull(i.normalized_status, ifNull(i.raw_status, ''))"
	severityExpression := "ifNull(i.normalized_severity, ifNull(i.raw_severity, ''))"
	if timeBound.Active {
		statusExpression = "if(i.resolved_at IS NOT NULL AND i.resolved_at <= " + timeBound.AsOfExpression() +
			", " + statusExpression + ", 'open')"
		// Selected as a constant empty string rather than dropped from the
		// SELECT, so the scan shape stays identical on both axes -- one
		// fewer place for the two paths to drift.
		severityExpression = "''"
	}
	statement := WithRowLimit(`SELECT i.id, `+statusExpression+`, `+severityExpression+`
FROM operational_incidents AS i FINAL
WHERE i.org_id = {org_id:String} AND i.id IN {ids:Array(String)} AND i.is_deleted = 0`+timeBound.ExistencePredicate("i.started_at"), DefaultRowLimit)

	var rows []IncidentRow
	err := QueryOrgScoped(ctx, client, "ReadIncidents", statement, orgID, ids, func(row RowScanner) error {
		var r IncidentRow
		if scanErr := row.Scan(&r.ID, &r.Status, &r.Severity); scanErr != nil {
			return scanErr
		}
		rows = append(rows, r)
		return nil
	}, timeBound.Bindings()...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
