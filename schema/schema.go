// Package schema is the SINGLE declared snapshot of the
// production ClickHouse column types the Context Fabric readers depend on.
//
// It exists because the same type drift bit this codebase twice. CHAOS-3789
// found devhealthsource scanning git_pull_requests.number (UInt32) into an
// *int64, which clickhouse-go rejects outright -- every live row failed
// Scan. CHAOS-3781 codex round 2 then found the IDENTICAL defect surviving
// in devhealthfacts, a different package reading the same column. Both
// times the tests agreed with the bug, because each package hand-authored
// its own fixtures and modeled the column as int64.
//
// One declaration, imported by every guard and fixture, is what stops a
// third occurrence: a fixture cannot disagree with the parity guard when
// both are rendered from this map.
//
// The types are read directly off production ClickHouse via
// system.columns and must not be inferred from a neighbouring table --
// the subtypes genuinely diverge. work_items.created_at is DateTime64(3)
// while git_pull_requests.created_at is DateTime64(3, 'UTC'), and the
// operational_* tables use precision 6 where most use 3. A guessed type
// yields a test that passes against a schema production does not have,
// which is worse than a failing one. The one exception is a table not yet
// live anywhere to read from (team_cognitive_load_daily, CHAOS-4365 item
// 2), declared straight from its ops migration DDL instead -- see that
// entry's comment.
//
// SCOPE: only the columns the readers actually SELECT. Drift in a column
// nobody reads cannot break anything, and declaring all 279 columns of
// these tables would add churn without adding a guarantee.
//
// This package is imported only by tests. It is a normal package rather
// than a _test.go file so that BOTH devhealthsource and devhealthfacts can
// import it -- a declaration inside either package's external test package
// would be unreachable from the other.
package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Column is one column's name and its exact ClickHouse type string, in the
// form system.columns.type reports it.
type Column struct {
	Name string
	Type string
}

// ProductionColumns is the snapshot: table name -> the columns Context
// Fabric reads from it, with production types.
//
// Columns are listed in PRODUCTION POSITION ORDER, not alphabetically:
// a fixture rendered from this map is then a positional replica of the
// real table, so a seed that lists values without naming columns lands
// them where production would.
//
// Read from dev-health-clickhouse-1 (database `default`) on 2026-08-13;
// CHAOS-3833's embed-text columns (work_items type/native_team_key/
// project_name/labels, git_pull_requests.body, ci_pipeline_runs.
// pipeline_name, repos.tags, teams.project_keys,
// operational_incidents.description) read the same way on 2026-08-14.
// Regenerate with the query in this package's doc, and see
// devhealthsource's freshness test, which fails when production drifts
// from what is declared here.
var ProductionColumns = map[string][]Column{
	"backfill_log": {
		{Name: "job_id", Type: "String"},
		{Name: "org_id", Type: "String"},
		{Name: "chunk_index", Type: "UInt32"},
		{Name: "provider", Type: "String"},
		{Name: "items_synced", Type: "UInt32"},
		{Name: "duration_ms", Type: "UInt64"},
		{Name: "status", Type: "String"},
		{Name: "error_message", Type: "String"},
		{Name: "created_at", Type: "DateTime64(3)"},
	},
	"capacity_forecasts": {
		{Name: "forecast_id", Type: "String"},
		{Name: "computed_at", Type: "DateTime64(3, 'UTC')"},
		{Name: "team_id", Type: "Nullable(String)"},
		{Name: "work_scope_id", Type: "Nullable(String)"},
		{Name: "backlog_size", Type: "UInt32"},
		{Name: "p50_days", Type: "Nullable(UInt16)"},
		{Name: "throughput_mean", Type: "Float64"},
		{Name: "throughput_stddev", Type: "Float64"},
		{Name: "insufficient_history", Type: "UInt8"},
		{Name: "high_variance", Type: "UInt8"},
		{Name: "org_id", Type: "String"},
	},
	"ci_pipeline_runs": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "run_id", Type: "String"},
		{Name: "status", Type: "Nullable(String)"},
		{Name: "started_at", Type: "DateTime64(3, 'UTC')"},
		{Name: "finished_at", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "last_synced", Type: "DateTime64(3, 'UTC')"},
		{Name: "org_id", Type: "String"},
		// CHAOS-3833: pipeline_name sits at production position 9 --
		// AFTER org_id (8) and before branch (17); several unread columns
		// between them are omitted per this package's scope rule.
		{Name: "pipeline_name", Type: "Nullable(String)"},
		{Name: "branch", Type: "Nullable(String)"},
	},
	"compounding_risk_daily": {
		{Name: "org_id", Type: "String"},
		{Name: "day", Type: "Date"},
		{Name: "scope", Type: "Enum8('repo' = 1, 'team' = 2)"},
		{Name: "scope_id", Type: "String"},
		{Name: "compounding_risk", Type: "Nullable(Float64)"},
		{Name: "severity", Type: "Enum8('unknown' = 0, 'low' = 1, 'elevated' = 2, 'high' = 3)"},
		{Name: "computed_at", Type: "DateTime"},
	},
	"deployments": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "deployment_id", Type: "String"},
		{Name: "status", Type: "Nullable(String)"},
		{Name: "environment", Type: "Nullable(String)"},
		{Name: "started_at", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "finished_at", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "deployed_at", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "release_ref", Type: "String"},
		{Name: "release_ref_confidence", Type: "Float64"},
		{Name: "last_synced", Type: "DateTime64(3, 'UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"estimate_coverage_metrics_daily": {
		{Name: "day", Type: "Date"},
		{Name: "provider", Type: "String"},
		{Name: "work_scope_id", Type: "String"},
		{Name: "team_id", Type: "Nullable(String)"},
		{Name: "estimated_count", Type: "UInt32"},
		{Name: "unestimated_count", Type: "UInt32"},
		{Name: "backlog_size", Type: "UInt32"},
		{Name: "ratio", Type: "Nullable(Float64)"},
		{Name: "computed_at", Type: "DateTime64(3, 'UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"git_pull_request_reviews": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "number", Type: "UInt32"},
		{Name: "review_id", Type: "String"},
		{Name: "state", Type: "String"},
		{Name: "submitted_at", Type: "DateTime64(3, 'UTC')"},
		{Name: "last_synced", Type: "DateTime64(3, 'UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"git_pull_requests": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "number", Type: "UInt32"},
		{Name: "title", Type: "Nullable(String)"},
		// CHAOS-3833: body is production position 4, between title and
		// state.
		{Name: "body", Type: "Nullable(String)"},
		{Name: "state", Type: "Nullable(String)"},
		{Name: "created_at", Type: "DateTime64(3, 'UTC')"},
		{Name: "merged_at", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "closed_at", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "head_branch", Type: "Nullable(String)"},
		{Name: "base_branch", Type: "Nullable(String)"},
		{Name: "last_synced", Type: "DateTime64(3, 'UTC')"},
		{Name: "org_id", Type: "String"},
	},
	// ic_landscape_rolling_30d is CHAOS-4364's LandscapeProvider's read
	// (landscape.go) -- its first reader in this package. repo_id and
	// identity_id are unread by the provider but ARE part of the live
	// ORDER BY (see EngineFull below), so CREATE TABLE requires them
	// declared regardless of read-scope, same as team_project_ownership's
	// source/valid_from. Read live off the kiac trial ClickHouse
	// (system.columns via kubectl exec, 2026-08-27).
	"ic_landscape_rolling_30d": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "as_of_day", Type: "Date"},
		{Name: "identity_id", Type: "String"},
		{Name: "team_id", Type: "LowCardinality(String)"},
		{Name: "map_name", Type: "LowCardinality(String)"},
		{Name: "churn_loc_30d", Type: "UInt64"},
		{Name: "delivery_units_30d", Type: "UInt32"},
		{Name: "cycle_p50_30d_hours", Type: "Float64"},
		{Name: "wip_max_30d", Type: "UInt32"},
		{Name: "computed_at", Type: "DateTime"},
		{Name: "org_id", Type: "String"},
	},
	"investment_metrics_daily": {
		{Name: "day", Type: "Date"},
		{Name: "team_id", Type: "LowCardinality(Nullable(String))"},
		{Name: "investment_area", Type: "LowCardinality(String)"},
		{Name: "project_stream", Type: "LowCardinality(String)"},
		{Name: "delivery_units", Type: "UInt32"},
		{Name: "work_items_completed", Type: "UInt32"},
		{Name: "prs_merged", Type: "UInt32"},
		{Name: "churn_loc", Type: "UInt64"},
		{Name: "cycle_p50_hours", Type: "Float64"},
		{Name: "computed_at", Type: "DateTime"},
		{Name: "org_id", Type: "String"},
	},
	"operational_incidents": {
		{Name: "org_id", Type: "String"},
		{Name: "source_version_at", Type: "DateTime64(6, 'UTC')"},
		{Name: "id", Type: "String"},
		{Name: "source_url", Type: "Nullable(String)"},
		{Name: "source_event_at", Type: "Nullable(DateTime64(6, 'UTC'))"},
		{Name: "observed_at", Type: "DateTime64(6, 'UTC')"},
		{Name: "last_synced", Type: "DateTime64(6, 'UTC')"},
		{Name: "relationship_confidence", Type: "Nullable(Float64)"},
		{Name: "raw_status", Type: "Nullable(String)"},
		{Name: "raw_severity", Type: "Nullable(String)"},
		{Name: "normalized_status", Type: "Nullable(String)"},
		{Name: "normalized_severity", Type: "Nullable(String)"},
		{Name: "service_id", Type: "Nullable(String)"},
		{Name: "title", Type: "String"},
		// CHAOS-3833: description is production position 26, between
		// title (25) and started_at (27). 0% populated in live data today
		// but the natural incident payload when a real provider ships --
		// see the incident template in the embed-text spec.
		{Name: "description", Type: "Nullable(String)"},
		{Name: "started_at", Type: "Nullable(DateTime64(6, 'UTC'))"},
		{Name: "resolved_at", Type: "Nullable(DateTime64(6, 'UTC'))"},
		{Name: "is_deleted", Type: "UInt8"},
		{Name: "deleted_at", Type: "Nullable(DateTime64(6, 'UTC'))"},
	},
	"operational_service_repository_mappings": {
		{Name: "org_id", Type: "String"},
		{Name: "source_version_at", Type: "DateTime64(6, 'UTC')"},
		{Name: "id", Type: "String"},
		{Name: "relationship_provenance", Type: "Nullable(String)"},
		{Name: "relationship_confidence", Type: "Nullable(Float64)"},
		{Name: "service_id", Type: "String"},
		{Name: "repo_id", Type: "Nullable(UUID)"},
		{Name: "valid_from", Type: "Nullable(DateTime64(6, 'UTC'))"},
		{Name: "valid_to", Type: "Nullable(DateTime64(6, 'UTC'))"},
		{Name: "is_active", Type: "UInt8"},
	},
	"recommendations_daily": {
		{Name: "team_id", Type: "LowCardinality(String)"},
		{Name: "org_id", Type: "String"},
		{Name: "rule_id", Type: "LowCardinality(String)"},
		{Name: "rule_version", Type: "LowCardinality(String)"},
		{Name: "window_start", Type: "Date"},
		{Name: "window_end", Type: "Date"},
		{Name: "fired", Type: "Bool"},
		{Name: "severity", Type: "LowCardinality(String)"},
		{Name: "title", Type: "String"},
		{Name: "rationale", Type: "String"},
		{Name: "success_criterion", Type: "String"},
		{Name: "computed_at", Type: "DateTime64(3, 'UTC')"},
	},
	"repo_metrics_daily": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "day", Type: "Date"},
		{Name: "commits_count", Type: "UInt32"},
		{Name: "prs_merged", Type: "UInt32"},
		{Name: "median_pr_cycle_hours", Type: "Float64"},
		{Name: "mttr_hours", Type: "Nullable(Float64)"},
		{Name: "change_failure_rate", Type: "Float64"},
		{Name: "computed_at", Type: "DateTime('UTC')"},
		{Name: "bus_factor", Type: "UInt32"},
		{Name: "code_ownership_gini", Type: "Float64"},
		{Name: "org_id", Type: "String"},
		// CHAOS-4364: FlowProvider's readRepositoryFlow (flow.go) reads
		// these five PR pickup/review-timing columns off the SAME table
		// metrics.go already declared above -- a distinct column set, no
		// conflict. Positions 11-15 live (system.columns, 2026-08-27).
		{Name: "prs_with_first_review", Type: "UInt32"},
		{Name: "pr_first_review_p50_hours", Type: "Nullable(Float64)"},
		{Name: "pr_first_review_p90_hours", Type: "Nullable(Float64)"},
		{Name: "pr_review_time_p50_hours", Type: "Nullable(Float64)"},
		{Name: "pr_pickup_time_p50_hours", Type: "Nullable(Float64)"},
	},
	// CHAOS-4365 item 2 (4347-C): team_cognitive_load_daily is a NEW table
	// (ops migration 081_team_cognitive_load_daily.sql) -- not yet live
	// anywhere to read types off of, so declared straight from that
	// migration's DDL instead of this package's usual production-read
	// convention. team_id is OWNERSHIP-resolved only (team_repo_ownership
	// merged over teams.repo_patterns) -- CHAOS-4321 hard rule -- never the
	// person->membership-fallback-tainted team_id column
	// user_metrics_daily/team_metrics_daily carry (CHAOS-4396).
	"team_cognitive_load_daily": {
		{Name: "org_id", Type: "String"},
		{Name: "team_id", Type: "String"},
		{Name: "day", Type: "Date"},
		{Name: "pr_interruption_load", Type: "Float64"},
		{Name: "context_spread_count", Type: "Float64"},
		{Name: "review_request_load", Type: "Float64"},
		{Name: "after_hours_commit_ratio", Type: "Nullable(Float64)"},
		{Name: "weekend_commit_ratio", Type: "Nullable(Float64)"},
		{Name: "contributing_repo_count", Type: "UInt32"},
		{Name: "sample_author_count", Type: "UInt32"},
		{Name: "computed_at", Type: "DateTime64(6, 'UTC')"},
	},
	// CHAOS-4347: team_metrics_daily/cicd_metrics_daily/deploy_metrics_daily
	// read live from the kiac trial ClickHouse (system.columns, 2026-08-26)
	// via `kubectl exec` into the trial-clickhouse pod (ns acr-trial-data),
	// not inferred from the ops migration files -- this package's own doc
	// comment requires reading types off production, and a migration file
	// does not show later ALTERs. Positions match `system.columns.position`
	// as read live.
	"team_metrics_daily": {
		{Name: "day", Type: "Date"},
		{Name: "team_id", Type: "LowCardinality(String)"},
		{Name: "team_name", Type: "String"},
		{Name: "commits_count", Type: "UInt32"},
		{Name: "after_hours_commits_count", Type: "UInt32"},
		{Name: "weekend_commits_count", Type: "UInt32"},
		{Name: "after_hours_commit_ratio", Type: "Float64"},
		{Name: "weekend_commit_ratio", Type: "Float64"},
		{Name: "computed_at", Type: "DateTime('UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"cicd_metrics_daily": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "day", Type: "Date"},
		{Name: "pipelines_count", Type: "UInt32"},
		{Name: "success_rate", Type: "Float64"},
		{Name: "avg_duration_minutes", Type: "Nullable(Float64)"},
		{Name: "p90_duration_minutes", Type: "Nullable(Float64)"},
		{Name: "avg_queue_minutes", Type: "Nullable(Float64)"},
		{Name: "computed_at", Type: "DateTime('UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"deploy_metrics_daily": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "day", Type: "Date"},
		{Name: "deployments_count", Type: "UInt32"},
		{Name: "failed_deployments_count", Type: "UInt32"},
		{Name: "deploy_time_p50_hours", Type: "Nullable(Float64)"},
		{Name: "lead_time_p50_hours", Type: "Nullable(Float64)"},
		{Name: "computed_at", Type: "DateTime('UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"repos": {
		{Name: "id", Type: "UUID"},
		{Name: "repo", Type: "String"},
		{Name: "ref", Type: "Nullable(String)"},
		{Name: "created_at", Type: "DateTime64(3, 'UTC')"},
		// CHAOS-3833: tags is production position 6, between created_at
		// (4; unread settings at 5 omitted) and last_synced (7). A JSON
		// array rendered as a string (e.g. `["github","Go"]`), parsed by
		// the producer -- NOT Array(String), unlike teams.project_keys.
		{Name: "tags", Type: "Nullable(String)"},
		{Name: "last_synced", Type: "DateTime64(3, 'UTC')"},
		{Name: "org_id", Type: "String"},
		{Name: "provider", Type: "String"},
	},
	"work_graph_deployment_incident_edges": {
		{Name: "edge_id", Type: "String"},
		{Name: "org_id", Type: "UUID"},
		{Name: "deployment_id", Type: "String"},
		{Name: "incident_id", Type: "String"},
		{Name: "repo_id", Type: "Nullable(UUID)"},
		{Name: "confidence", Type: "Float32"},
		{Name: "source", Type: "LowCardinality(String)"},
		{Name: "evidence", Type: "String"},
		{Name: "observed_at", Type: "DateTime64(3, 'UTC')"},
		{Name: "computed_at", Type: "DateTime64(3, 'UTC')"},
	},
	"work_item_dependencies": {
		{Name: "source_work_item_id", Type: "String"},
		{Name: "target_work_item_id", Type: "String"},
		{Name: "relationship_type", Type: "String"},
		{Name: "relationship_type_raw", Type: "String"},
		{Name: "last_synced", Type: "DateTime64(3)"},
		{Name: "org_id", Type: "String"},
	},
	// CHAOS-3802's four tables. Read off live system.columns the same way
	// every entry above was, and listed in production POSITION order with
	// only the columns this epic's readers SELECT -- plus every column
	// each table's sorting key names, which a CREATE TABLE cannot omit.
	//
	// teams.updated_at genuinely carries NO timezone qualifier --
	// DateTime64(6), not DateTime64(6,'UTC') -- while queryTeams'
	// sincePredicate binds a DateTime64(6,'UTC') parameter against it. The
	// Enum8 columns are spelled out in full for the same reason
	// git_pull_requests.number is UInt32 here: a fake cannot tell you
	// whether Scan survives the real type, and both are read through
	// toString().
	"teams": {
		{Name: "id", Type: "String"},
		{Name: "name", Type: "String"},
		{Name: "description", Type: "Nullable(String)"},
		{Name: "updated_at", Type: "DateTime64(6)"},
		{Name: "org_id", Type: "String"},
		{Name: "provider", Type: "String"},
		{Name: "native_team_key", Type: "Nullable(String)"},
		// CHAOS-3833: project_keys is production position 12, after
		// native_team_key (10; unread parent_team_id at 11 omitted) and
		// before is_active (14). A real Array(String), scanned as
		// []string.
		{Name: "project_keys", Type: "Array(String)"},
		{Name: "is_active", Type: "UInt8"},
	},
	// project_membership_transitions is ops migration 077 (CHAOS-4193/4194).
	// The FULL declared column set, not a reader-scoped subset: acr's own
	// producer never queries this table directly (it reads through the
	// project_membership_presence VIEW, ProjectMembershipPresenceViewDDL
	// below), but the view's CTEs reference every column here except actor/
	// source_id/last_synced, so an integration fixture that omitted any of
	// them would fail to even CREATE the view it exists to seed.
	"project_membership_transitions": {
		{Name: "org_id", Type: "String"},
		{Name: "source_id", Type: "Nullable(UUID)"},
		{Name: "repo_id", Type: "UUID"},
		{Name: "subject_kind", Type: "LowCardinality(String)"},
		{Name: "subject_id", Type: "String"},
		{Name: "provider", Type: "LowCardinality(String)"},
		{Name: "from_project_id", Type: "String"},
		{Name: "to_project_id", Type: "String"},
		{Name: "from_project_key", Type: "String"},
		{Name: "to_project_key", Type: "String"},
		{Name: "actor", Type: "String"},
		{Name: "occurred_at", Type: "DateTime64(3)"},
		{Name: "last_synced", Type: "DateTime64(3)"},
		{Name: "event_id", Type: "String"},
	},
	"projects": {
		{Name: "id", Type: "String"},
		{Name: "org_id", Type: "String"},
		{Name: "provider", Type: "String"},
		{Name: "project_key", Type: "Nullable(String)"},
		{Name: "name", Type: "String"},
		{Name: "is_active", Type: "UInt8"},
		{Name: "state", Type: "LowCardinality(String)"},
		{Name: "url", Type: "String"},
		{Name: "updated_at", Type: "DateTime64(3, 'UTC')"},
	},
	"work_item_team_attributions": {
		{Name: "org_id", Type: "String"},
		{Name: "repo_id", Type: "UUID"},
		{Name: "work_item_id", Type: "String"},
		{Name: "team_id", Type: "Nullable(String)"},
		{Name: "source", Type: "Enum8('native_team' = 1, 'linked_issue' = 2, 'project_ownership' = 3, 'repo_ownership' = 4, 'assignee_membership' = 5, 'unassigned' = 6, 'issue_project' = 7, 'manual_fallback' = 8)"},
		{Name: "is_primary", Type: "UInt8"},
		{Name: "confidence", Type: "Enum8('high' = 1, 'medium' = 2, 'low' = 3, 'manual' = 4, 'none' = 5)"},
		{Name: "computed_at", Type: "DateTime64(3, 'UTC')"},
	},
	"team_project_ownership": {
		{Name: "org_id", Type: "String"},
		{Name: "provider", Type: "String"},
		{Name: "team_id", Type: "String"},
		{Name: "project_id", Type: "String"},
		{Name: "project_key", Type: "Nullable(String)"},
		{Name: "source", Type: "Enum8('native' = 1, 'jira_legacy' = 2, 'provider_access' = 3, 'manual' = 4, 'inferred' = 5)"},
		{Name: "valid_from", Type: "DateTime64(3, 'UTC')"},
		{Name: "valid_to", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "updated_at", Type: "DateTime64(3, 'UTC')"},
	},
	// CHAOS-4363: read live off the kiac trial ClickHouse (system.columns,
	// 2026-08-27) via `kubectl exec` into the trial-clickhouse pod (ns
	// acr-trial-data), matching this package's own doc comment requirement
	// -- not inferred from an ops migration file. health.go's project-subject
	// rollup is the first reader of this table (repo layer, one hop past
	// team_project_ownership).
	"team_repo_ownership": {
		{Name: "org_id", Type: "String"},
		{Name: "provider", Type: "String"},
		{Name: "team_id", Type: "String"},
		{Name: "repo_id", Type: "Nullable(UUID)"},
		{Name: "repo_full_name", Type: "String"},
		{Name: "match_type", Type: "Enum8('exact' = 1, 'pattern' = 2)"},
		{Name: "source", Type: "Enum8('native' = 1, 'jira_legacy' = 2, 'provider_access' = 3, 'manual' = 4, 'inferred' = 5)"},
		{Name: "is_primary", Type: "UInt8"},
		{Name: "specificity", Type: "UInt16"},
		{Name: "priority", Type: "Int32"},
		{Name: "valid_from", Type: "DateTime64(3, 'UTC')"},
		{Name: "valid_to", Type: "Nullable(DateTime64(3, 'UTC'))"},
		{Name: "updated_at", Type: "DateTime64(3, 'UTC')"},
	},
	// work_item_metrics_daily is CHAOS-4364's FlowProvider's read (flow.go),
	// its first reader in this package. provider is unread by the provider
	// but IS part of the live ORDER BY (EngineFull below), so declared
	// regardless, same as ic_landscape_rolling_30d's repo_id/identity_id
	// above. Read live off the kiac trial ClickHouse (2026-08-27).
	"work_item_metrics_daily": {
		{Name: "day", Type: "Date"},
		{Name: "provider", Type: "LowCardinality(String)"},
		{Name: "work_scope_id", Type: "LowCardinality(String)"},
		{Name: "team_id", Type: "LowCardinality(String)"},
		{Name: "items_started", Type: "UInt32"},
		{Name: "items_completed", Type: "UInt32"},
		{Name: "wip_count_end_of_day", Type: "UInt32"},
		{Name: "cycle_time_p50_hours", Type: "Nullable(Float64)"},
		{Name: "cycle_time_p90_hours", Type: "Nullable(Float64)"},
		{Name: "lead_time_p50_hours", Type: "Nullable(Float64)"},
		{Name: "lead_time_p90_hours", Type: "Nullable(Float64)"},
		{Name: "wip_age_p50_hours", Type: "Nullable(Float64)"},
		{Name: "wip_age_p90_hours", Type: "Nullable(Float64)"},
		{Name: "bug_completed_ratio", Type: "Float64"},
		{Name: "story_points_completed", Type: "Float64"},
		{Name: "computed_at", Type: "DateTime('UTC')"},
		{Name: "org_id", Type: "String"},
	},
	"work_items": {
		{Name: "repo_id", Type: "UUID"},
		{Name: "work_item_id", Type: "String"},
		// CHAOS-4193: project_membership_presence's work_item_column arm
		// selects w.provider (009_raw_work_items.sql declares it at
		// production position 3, directly after work_item_id); never read
		// by acr before this producer joined through the view.
		{Name: "provider", Type: "String"},
		{Name: "title", Type: "String"},
		// CHAOS-3833: type is production position 6 (title is 4; unread
		// description at 5 omitted), before status (7).
		{Name: "type", Type: "String"},
		{Name: "status", Type: "String"},
		// CHAOS-4193: project_membership_presence's work_item_column arm
		// selects w.project_key too -- production position 9, directly
		// before project_id (10). Distinct from projects.project_key (the
		// dual-arm join target); this is the WORK ITEM row's own carried
		// copy of it.
		{Name: "project_key", Type: "String"},
		// CHAOS-3802: queryWorkItemProjects selects project_id and joins it
		// to projects (CHAOS-4108: on projects.id OR projects.project_key,
		// not id alone). Position 10 in production, i.e. before created_at.
		{Name: "project_id", Type: "String"},
		// CHAOS-3833: native_team_key (11) and project_name (12) follow
		// project_id (10) directly in production.
		{Name: "native_team_key", Type: "String"},
		{Name: "project_name", Type: "String"},
		{Name: "created_at", Type: "DateTime64(3)"},
		{Name: "updated_at", Type: "DateTime64(3)"},
		{Name: "completed_at", Type: "Nullable(DateTime64(3))"},
		{Name: "closed_at", Type: "Nullable(DateTime64(3))"},
		// CHAOS-3833: labels is production position 20, between closed_at
		// (19) and parent_id (24; unread story_points/sprint_* between
		// them omitted). A real Array(String), scanned as []string.
		{Name: "labels", Type: "Array(String)"},
		{Name: "parent_id", Type: "String"},
		{Name: "url", Type: "String"},
		{Name: "last_synced", Type: "DateTime64(3)"},
		{Name: "org_id", Type: "String"},
	},
}

// EngineFull is each table's COMPLETE physical definition, exactly as
// system.tables.engine_full reports it: engine, version column, PARTITION
// BY, ORDER BY and SETTINGS in one string.
//
// CHAOS-3781 round-4 R4-1: this replaces separate hand-maintained Engines
// and OrderBy maps that carried only the engine CLASS. Dropping
// ReplacingMergeTree's VERSION column changed dedup semantics -- FINAL on
// a versionless table keeps an arbitrary row among those sharing a sort
// key, while production keeps the one with the highest version. Any
// fixture built from the class alone was therefore proving the wrong
// thing about exactly the FINAL behaviour several providers depend on.
//
// It is ONE field on purpose. The engine class, the version column and the
// sorting key are all facets of a single physical definition, and the
// three previous rounds each found a different hand-authored facet drifted
// from live. A field that cannot be authored separately cannot drift
// separately.
var EngineFull = map[string]string{
	"backfill_log":                            "MergeTree ORDER BY (org_id, job_id, chunk_index) SETTINGS index_granularity = 8192",
	"capacity_forecasts":                      "ReplacingMergeTree(computed_at) ORDER BY (org_id, forecast_id) SETTINGS index_granularity = 8192",
	"ci_pipeline_runs":                        "ReplacingMergeTree(last_synced) ORDER BY (org_id, repo_id, run_id) SETTINGS index_granularity = 8192",
	"cicd_metrics_daily":                      "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, repo_id, day) SETTINGS index_granularity = 8192",
	"compounding_risk_daily":                  "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, scope, scope_id, day, computed_at) SETTINGS index_granularity = 8192",
	"deploy_metrics_daily":                    "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, repo_id, day) SETTINGS index_granularity = 8192",
	"deployments":                             "ReplacingMergeTree(last_synced) ORDER BY (org_id, repo_id, deployment_id) SETTINGS index_granularity = 8192",
	"estimate_coverage_metrics_daily":         "ReplacingMergeTree(computed_at) PARTITION BY toYYYYMM(day) ORDER BY (org_id, day, provider, work_scope_id, ifNull(team_id, '')) SETTINGS index_granularity = 8192",
	"git_pull_request_reviews":                "ReplacingMergeTree(last_synced) ORDER BY (org_id, repo_id, number, review_id) SETTINGS index_granularity = 8192",
	"git_pull_requests":                       "ReplacingMergeTree(last_synced) ORDER BY (org_id, repo_id, number) SETTINGS index_granularity = 8192",
	"ic_landscape_rolling_30d":                "ReplacingMergeTree(computed_at) PARTITION BY toYYYYMM(as_of_day) ORDER BY (org_id, repo_id, team_id, map_name, as_of_day, identity_id) SETTINGS index_granularity = 8192",
	"investment_metrics_daily":                "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, day, team_id, investment_area, project_stream) SETTINGS allow_nullable_key = 1, index_granularity = 8192",
	"operational_incidents":                   "ReplacingMergeTree(source_version_at) ORDER BY (org_id, id) SETTINGS index_granularity = 8192",
	"project_membership_transitions":          "ReplacingMergeTree(last_synced) ORDER BY (org_id, subject_kind, repo_id, subject_id, occurred_at, event_id) SETTINGS index_granularity = 8192",
	"projects":                                "ReplacingMergeTree(updated_at) ORDER BY (org_id, provider, id) SETTINGS index_granularity = 8192",
	"operational_service_repository_mappings": "ReplacingMergeTree(source_version_at) ORDER BY (org_id, id) SETTINGS index_granularity = 8192",
	"recommendations_daily":                   "ReplacingMergeTree(computed_at) PARTITION BY toYYYYMM(window_end) ORDER BY (org_id, team_id, rule_id, window_end) SETTINGS index_granularity = 8192",
	"repo_metrics_daily":                      "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, repo_id, day) SETTINGS index_granularity = 8192",
	"repos":                                   "ReplacingMergeTree(last_synced) ORDER BY (org_id, id) SETTINGS index_granularity = 8192",
	// CHAOS-3802: valid_from is IN team_project_ownership's sorting key, so
	// FINAL cannot collapse two windows of the same edge -- that is exactly
	// what makes queryProjectTeams' GROUP BY load-bearing (its Trap C note).
	// work_item_team_attributions keys on ifNull(team_id, ''), not team_id:
	// two attributions differing only in team are DISTINCT rows in
	// production, and a fixture that dropped the term would collapse them.
	"team_cognitive_load_daily":            "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, team_id, day) SETTINGS index_granularity = 8192",
	"team_metrics_daily":                   "MergeTree PARTITION BY toYYYYMM(day) ORDER BY (org_id, team_id, day) SETTINGS index_granularity = 8192",
	"team_project_ownership":               "ReplacingMergeTree(updated_at) ORDER BY (org_id, provider, project_id, team_id, source, valid_from) SETTINGS index_granularity = 8192",
	"team_repo_ownership":                  "ReplacingMergeTree(updated_at) ORDER BY (org_id, provider, repo_full_name, team_id, source, valid_from) SETTINGS index_granularity = 8192",
	"teams":                                "ReplacingMergeTree(updated_at) ORDER BY (org_id, id) SETTINGS index_granularity = 8192",
	"work_graph_deployment_incident_edges": "ReplacingMergeTree(computed_at) PARTITION BY toYYYYMM(observed_at) ORDER BY (org_id, deployment_id, incident_id, source) SETTINGS index_granularity = 8192",
	"work_item_dependencies":               "ReplacingMergeTree(last_synced) ORDER BY (org_id, source_work_item_id, target_work_item_id, relationship_type) SETTINGS index_granularity = 8192",
	"work_item_metrics_daily":              "ReplacingMergeTree(computed_at) PARTITION BY toYYYYMM(day) ORDER BY (org_id, provider, day, work_scope_id, team_id) SETTINGS index_granularity = 8192",
	"work_item_team_attributions":          "ReplacingMergeTree(computed_at) ORDER BY (org_id, repo_id, work_item_id, ifNull(team_id, ''), source) SETTINGS index_granularity = 8192",
	"work_items":                           "ReplacingMergeTree(last_synced) ORDER BY (org_id, repo_id, work_item_id) SETTINGS index_granularity = 8192",
}

// DDL renders CREATE TABLE statements for the named tables, in a
// deterministic order so a failure message is stable. A caller passing no
// names gets every declared table.
//
// Each table uses its PRODUCTION engine (see Engines), not a uniform one.
// That is load-bearing rather than cosmetic: the readers query the
// ReplacingMergeTree tables with FINAL, and FINAL against a plain
// MergeTree is a query error -- so a fixture that simplified the engine
// would fail every provider for a reason that has nothing to do with the
// type parity under test, which is exactly the false red it produced when
// first written.
func DDL(tables ...string) []string {
	if len(tables) == 0 {
		for table := range ProductionColumns {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	statements := make([]string, 0, len(tables))
	for _, table := range tables {
		columns, ok := ProductionColumns[table]
		if !ok {
			panic("devhealthschema: no declared columns for table " + table)
		}
		rendered := make([]string, 0, len(columns))
		for _, column := range columns {
			rendered = append(rendered, column.Name+" "+column.Type)
		}
		engine, ok := EngineFull[table]
		if !ok {
			panic("devhealthschema: no declared engine for table " + table)
		}
		statements = append(statements, fmt.Sprintf(
			"CREATE TABLE %s (%s) ENGINE = %s",
			table, strings.Join(rendered, ", "), withNullableKeySetting(engine)))
	}
	return statements
}

// withNullableKeySetting appends allow_nullable_key to a definition's
// SETTINGS. Several production sort keys are Nullable (team_id,
// work_scope_id); the fixture carries the DECLARED types rather than
// altering them to satisfy the default, because altering them would
// rebuild the exact drift these guards exist to catch.
func withNullableKeySetting(engineFull string) string {
	if strings.Contains(engineFull, "allow_nullable_key") {
		return engineFull
	}
	if strings.Contains(engineFull, "SETTINGS") {
		return engineFull + ", allow_nullable_key = 1"
	}
	return engineFull + " SETTINGS allow_nullable_key = 1"
}

// ProjectMembershipPresenceViewDDL is the verbatim CREATE OR REPLACE VIEW
// statement from ops migration 077
// (dev-health-ops/src/dev_health_ops/migrations/clickhouse/077_project_membership_transitions.sql),
// copied byte-for-byte (comments stripped; the executable SQL is
// unchanged) rather than derived, because DDL() only ever renders a plain
// CREATE TABLE from a column list and this object's CTEs/arrayJoin/UNION
// ALL shape has no such mechanical rendering -- ops's own migration file
// is the only source of truth for it. A future edit to 077's view body
// must update this constant too, or the view devhealthsource's
// integration tests build against will silently disagree with what
// production actually runs.
const ProjectMembershipPresenceViewDDL = `CREATE OR REPLACE VIEW project_membership_presence AS
WITH touched AS (
    SELECT
        org_id,
        subject_kind,
        repo_id,
        subject_id,
        provider,
        occurred_at,
        event_id,
        to_project_id,
        arrayJoin(arrayFilter(
            pair -> pair.1 != '',
            if(from_project_id = to_project_id,
               [(to_project_id, to_project_key)],
               [(to_project_id, to_project_key), (from_project_id, from_project_key)])
        )) AS touch
    FROM project_membership_transitions FINAL
),
latest_membership AS (
    SELECT
        org_id,
        subject_kind,
        repo_id,
        subject_id,
        touch.1 AS project_id,
        argMax(touch.2, (occurred_at, event_id)) AS project_key,
        argMax(provider, (occurred_at, event_id)) AS provider,
        argMax(to_project_id, (occurred_at, event_id)) AS latest_to_project_id,
        max(occurred_at) AS observed_at
    FROM touched
    GROUP BY org_id, subject_kind, repo_id, subject_id, project_id
),
subjects_with_history AS (
    SELECT DISTINCT org_id, subject_kind, repo_id, subject_id
    FROM project_membership_transitions FINAL
)
SELECT
    org_id,
    subject_kind,
    repo_id,
    subject_id,
    provider,
    project_id,
    project_key,
    observed_at,
    'transition' AS source
FROM latest_membership
WHERE latest_to_project_id = project_id
UNION ALL
SELECT
    w.org_id AS org_id,
    'work_item' AS subject_kind,
    w.repo_id AS repo_id,
    w.work_item_id AS subject_id,
    w.provider AS provider,
    w.project_id AS project_id,
    w.project_key AS project_key,
    w.updated_at AS observed_at,
    'work_item_column' AS source
FROM work_items AS w FINAL
WHERE w.project_id != ''
    AND w.provider != 'gitlab'
    AND (w.provider != 'github' OR startsWith(w.project_id, 'ghprojv2:'))
    AND (w.org_id, 'work_item', w.repo_id, w.work_item_id) NOT IN (
        SELECT org_id, subject_kind, repo_id, subject_id FROM subjects_with_history
    )`
