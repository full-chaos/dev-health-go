package readers_test

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

func TestCIRunsOneClickHouseVersion(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*clickhouse:\s*\[([^\]]*)\]`).FindAllStringSubmatch(string(raw), -1) {
		for _, v := range regexp.MustCompile(`['"]?(\d+\.\d+)['"]?`).FindAllStringSubmatch(m[1], -1) {
			seen[v[1]] = true
		}
	}
	for _, m := range regexp.MustCompile(`clickhouse-server:(\d+\.\d+)`).FindAllStringSubmatch(string(raw), -1) {
		seen[m[1]] = true
	}
	var versions []string
	for v := range seen {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	if len(versions) != 1 {
		t.Fatalf("workflow runs ClickHouse versions %v, want exactly one", versions)
	}
}
