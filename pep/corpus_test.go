package pep

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pepCorpusOpsCommit is the dev-health-ops commit the authorization corpora in
// testdata were copied from. scripts/sync-corpus.sh rewrites testdata/SOURCE and
// the corpus files; the same run rewrites this constant, so they move together.
const pepCorpusOpsCommit = "f2e8cee05f6e4a512a991652fbe6e2369dfd26f9"

func TestCorpusSourceIsPinned(t *testing.T) {
	file, err := os.Open("testdata/SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pinned := map[string]string{}
	commits := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "ops_commit "); ok {
			if value != pepCorpusOpsCommit {
				t.Fatalf("SOURCE ops_commit is %q, the corpus is pinned to %q: a corpus sync updates pepCorpusOpsCommit, testdata/SOURCE and the corpus together", value, pepCorpusOpsCommit)
			}
			commits++
			continue
		}
		if strings.HasPrefix(line, "ops_path ") {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("bad SOURCE line %q", line)
		}
		pinned[name] = sum
	}
	if commits != 1 {
		t.Fatalf("SOURCE must hold exactly one ops_commit line, has %d", commits)
	}
	var onDisk []string
	err = filepath.Walk("testdata", func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel("testdata", path)
			if rel != "SOURCE" {
				onDisk = append(onDisk, rel)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(onDisk)
	if len(onDisk) != len(pinned) {
		t.Errorf("testdata holds %d files, SOURCE pins %d", len(onDisk), len(pinned))
	}
	for _, name := range onDisk {
		want, ok := pinned[name]
		if !ok {
			t.Errorf("%s is on disk but SOURCE does not pin it", name)
			continue
		}
		sum := sha256.Sum256(readFile(t, name))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: sha256 %s, SOURCE pins %s", name, got, want)
		}
	}
}
