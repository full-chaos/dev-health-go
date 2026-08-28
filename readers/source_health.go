package readers

import "context"

// SourceHealthRow is one row of backfill_log's latest per-provider
// ingestion outcome.
type SourceHealthRow struct {
	Provider    string
	Status      string
	ItemsSynced int64
	// DurationMS is scanned as uint64, matching backfill_log.duration_ms's
	// production column type -- NOT wrapped with toInt64 in SQL, because
	// that silently turns a value above MaxInt64 negative. A caller that
	// needs it as an int64 must range-check it after this reader hands the
	// value back (see acr devhealthfacts's representableInt64), never
	// assume it fits.
	DurationMS   uint64
	ErrorMessage string
	CreatedAt    string
}

// ReadSourceHealth reads backfill_log -- the per-provider ingestion job
// outcome Dev Health Ops records for every backfill/sync run (status,
// duration, items synced, and any error). This query is org-scoped only:
// backfill_log has no per-subject key other than org_id itself, so ids is
// bound (via QueryOrgScoped) but not referenced in the WHERE clause below
// -- there is nothing else to scope an "ids IN (...)" clause against, and
// WHERE org_id = {org_id:String} is itself the whole scope.
//
// row_number() OVER (PARTITION BY provider ORDER BY created_at DESC) picks
// each provider's single most recent ingestion job outcome. created_at
// DESC alone is not a TOTAL order (Codex round-2 finding M1) -- two
// backfill jobs for the same provider could start in the same second.
// backfill_log's real sorting key is (org_id, job_id, chunk_index) --
// job_id alone is NOT per-row unique, a single backfill job is chunked
// into several rows sharing one job_id (Codex round-3 correction) -- so
// this reader tiebreaks on job_id, then chunk_index, both real columns, no
// value hash needed.
//
// toInt64 on items_synced (UInt32): the clickhouse-go driver rejects
// scanning either a UInt32 or a UInt64 into an *int64 destination, the
// same class of defect CHAOS-3789 fixed for git_pull_requests.number.
// duration_ms is UInt64 and is deliberately NOT wrapped with toInt64 (see
// the DurationMS field doc above) -- it is scanned raw and left for the
// caller to range-check.
func ReadSourceHealth(ctx context.Context, client QueryClient, orgID string, ids []string, timeBound TimeBound) ([]SourceHealthRow, error) {
	statement := WithRowLimit(`SELECT provider, status, toInt64(items_synced), duration_ms, error_message, toString(created_at)
FROM (
	SELECT provider, status, items_synced, duration_ms, error_message, created_at,
		row_number() OVER (PARTITION BY provider ORDER BY created_at DESC, job_id DESC, chunk_index DESC) AS rn
	FROM backfill_log
	WHERE org_id = {org_id:String}`+timeBound.TimestampPredicate("created_at")+`
)
WHERE rn = 1`, DefaultRowLimit)

	var rows []SourceHealthRow
	err := QueryOrgScoped(ctx, client, "ReadSourceHealth", statement, orgID, ids, func(row RowScanner) error {
		var r SourceHealthRow
		if scanErr := row.Scan(&r.Provider, &r.Status, &r.ItemsSynced, &r.DurationMS, &r.ErrorMessage, &r.CreatedAt); scanErr != nil {
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
