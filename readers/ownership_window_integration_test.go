//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/full-chaos/dev-health-go/readers"
)

func TestIntegrationProjectOwnershipIsNotTimeSlicedByTheSyncStamp(t *testing.T) {
	client, seed := tmClient(t)
	ctx := context.Background()
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -60)
	cases := []struct {
		name      string
		validFrom string
		validTo   string
		bound     readers.TimeBound
		want      int
	}{
		{"range, synced after window end", "2026-09-01 00:00:00", "NULL", readers.TimeBound{Active: true, HasStart: true, Start: start, End: end}, 1},
		{"point, synced after instant", "2026-09-01 00:00:00", "NULL", readers.TimeBound{Active: true, End: end}, 1},
		{"range, ended before window start", "2026-01-01 00:00:00", "'2026-03-01 00:00:00'", readers.TimeBound{Active: true, HasStart: true, Start: start, End: end}, 0},
		{"range, ended inside window", "2026-01-01 00:00:00", "'2026-05-01 00:00:00'", readers.TimeBound{Active: true, HasStart: true, Start: start, End: end}, 1},
		{"point, ended before instant", "2026-01-01 00:00:00", "'2026-05-01 00:00:00'", readers.TimeBound{Active: true, End: end}, 0},
		{"unbounded, synced in the past", "2026-01-01 00:00:00", "NULL", readers.TimeBound{}, 1},
		{"unbounded, ended", "2026-01-01 00:00:00", "'2026-05-01 00:00:00'", readers.TimeBound{}, 0},
	}
	for i, c := range cases {
		team := fmt.Sprintf("own-%d", i)
		for _, stmt := range []string{
			fmt.Sprintf("INSERT INTO projects (id, org_id, provider, project_key, name, is_active, state, url, updated_at) VALUES ('proj-%s', '%s', 'linear', NULL, 'p', 1, 'started', '', '2026-01-01 00:00:00')", team, tmOrg),
			fmt.Sprintf("INSERT INTO team_project_ownership (org_id, provider, team_id, project_id, project_key, source, valid_from, valid_to, updated_at) VALUES ('%s', 'linear', '%s', 'proj-%s', NULL, 'native', '%s', %s, '2026-01-01 00:00:00')", tmOrg, team, team, c.validFrom, c.validTo),
			fmt.Sprintf("INSERT INTO teams (id, org_id, name, updated_at) VALUES ('%s', '%s', 'Team %s', '2026-01-01 00:00:00')", team, tmOrg, team),
			tmSeedInsert(team, tmRow{repo: tmRepo(i), day: "2026-05-20", at: "2026-05-21 00:00:00", commits: 4, ah: 1, wk: 1}),
		} {
			if err := seed.Exec(ctx, stmt); err != nil {
				t.Fatalf("seed %q: %v", stmt, err)
			}
		}
		rows, err := readers.ReadProjectMetricsBreakdown(ctx, client, tmOrg, []string{"linear:proj-" + team}, c.bound)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(rows) != c.want {
			t.Errorf("%s: rows = %d, want %d", c.name, len(rows), c.want)
		}
	}
}
