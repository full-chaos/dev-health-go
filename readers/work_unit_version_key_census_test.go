package readers_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/full-chaos/dev-health-go/latestrow"
	"github.com/full-chaos/dev-health-go/readers"
)

var workUnitVersionColumns = []string{"from_ts", "to_ts", "theme_distribution_json", "subcategory_distribution_json", "structural_evidence_json"}

// workUnitVersionSites names every pick of a work_unit_investments version in
// this module: 5 columns in each of 2 files, 10 sites. A new pick adds its row
// here and reads latestrow; it never builds its own key.
var workUnitVersionSites = map[string][]string{
	"readers/investment_theme.go":         workUnitVersionColumns,
	"readers/investment_project_theme.go": workUnitVersionColumns,
}

var (
	bareComputedAtPick = regexp.MustCompile(`argM(?:ax|in)(?:If)?\(\s*[^()]*,\s*(?:work_unit_investments\.)?computed_at\s*[,)]`)
	latestrowCall      = regexp.MustCompile(`latestrow\.ArgMax(?:KeepNull(?:By)?)?\(`)
	ownKeyMarkers      = []string{"_part_offset", "splitByChar('_'", "._part)", "OrderKey ="}
)

func moduleSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("..", path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkUnitVersionKeyCensus(t *testing.T) {
	sources := moduleSources(t)
	for file, columns := range workUnitVersionSites {
		src, ok := sources[file]
		if !ok {
			t.Fatalf("census site file %s is missing", file)
		}
		for _, column := range columns {
			if n := strings.Count(src, `latestrow.ArgMax("`+column+`")`); n != 1 {
				t.Errorf("%s: latestrow.ArgMax(%q) appears %d times, want 1", file, column, n)
			}
		}
	}
	want := 0
	for _, columns := range workUnitVersionSites {
		want += len(columns)
	}
	got := 0
	for file, src := range sources {
		got += len(latestrowCall.FindAllString(src, -1))
		if file == "latestrow/latestrow.go" {
			continue
		}
		for _, marker := range ownKeyMarkers {
			if strings.Contains(src, marker) {
				t.Errorf("%s builds its own version key (%q); read latestrow instead", file, marker)
			}
		}
		if strings.Contains(src, "FROM work_unit_investments") {
			for _, hit := range bareComputedAtPick.FindAllString(src, -1) {
				t.Errorf("%s picks a work_unit_investments version by computed_at alone: %s", file, hit)
			}
			if strings.Contains(src, "work_unit_investments FINAL") || strings.Contains(src, "LIMIT 1 BY") {
				t.Errorf("%s picks a work_unit_investments version outside latestrow (FINAL / LIMIT 1 BY)", file)
			}
		}
	}
	if got != want {
		t.Errorf("module holds %d latestrow picks, census names %d: add the new site to workUnitVersionSites", got, want)
	}
}

func TestWorkUnitVersionPicksRenderTheSharedKey(t *testing.T) {
	ctx := context.Background()
	team := &fakeClient{}
	if _, err := readers.ReadTeamThemeMix(ctx, team, "org", []string{"team"}, readers.TimeBound{}); err != nil {
		t.Fatal(err)
	}
	project := &fakeClient{}
	if _, err := readers.ReadProjectThemeMix(ctx, project, "org", []string{"linear:p"}, readers.TimeBound{}); err != nil {
		t.Fatal(err)
	}
	for reader, client := range map[string]*fakeClient{"ReadTeamThemeMix": team, "ReadProjectThemeMix": project} {
		if len(client.queries) != 1 {
			t.Fatalf("%s ran %d statements, want 1", reader, len(client.queries))
		}
		statement := client.queries[0].statement
		for _, column := range workUnitVersionColumns {
			if n := strings.Count(statement, latestrow.ArgMax(column)+" AS "+column); n != 1 {
				t.Errorf("%s: %s AS %s appears %d times, want 1", reader, latestrow.ArgMax(column), column, n)
			}
		}
		if n := strings.Count(statement, latestrow.OrderKey); n != len(workUnitVersionColumns) {
			t.Errorf("%s: OrderKey appears %d times, want %d", reader, n, len(workUnitVersionColumns))
		}
		if hit := bareComputedAtPick.FindString(statement); hit != "" {
			t.Errorf("%s still picks by computed_at alone: %s", reader, hit)
		}
	}
}
