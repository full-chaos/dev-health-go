package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The corpus pin tests in authverify require ops_commit in the SOURCE files to
// equal two constants in the test files. These tests run scripts/sync-corpus.sh
// against a throwaway ops repository and a copy of the files it edits, so the
// script is proven to move every pin together and to refuse before touching
// anything when a pin constant is missing.

const (
	accessTokenTest = "authverify/accesstoken_test.go"
	workloadTest    = "authverify/workload_token_test.go"
	pepTest         = "pep/corpus_test.go"
	accessConst     = "corpusOpsCommit"
	workloadConst   = "workloadCorpusOpsCommit"
	pepConst        = "pepCorpusOpsCommit"
)

var pepExampleSets = []string{"authorization_request", "authorization_batch_request", "authorization_decision", "authorization_batch_response"}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// opsFixture is a source repository, its bare origin and a clone of it, laid
// out like the four dev-health-ops paths the script copies.
type opsFixture struct {
	src, clone string
}

func newOps(t *testing.T, base string) opsFixture {
	t.Helper()
	f := opsFixture{src: filepath.Join(base, "src"), clone: filepath.Join(base, "ops")}
	origin := filepath.Join(base, "origin.git")
	run(t, base, "git", "init", "-q", "--bare", "-b", "main", origin)
	run(t, base, "git", "init", "-q", "-b", "main", f.src)
	run(t, f.src, "git", "remote", "add", "origin", origin)
	f.commit(t, "one")
	run(t, base, "git", "clone", "-q", origin, f.clone)
	return f
}

func (f opsFixture) commit(t *testing.T, tag string) string {
	t.Helper()
	golden := filepath.Join(f.src, "contracts/auth/v1/golden")
	examples := filepath.Join(f.src, "contracts/auth/v1/examples")
	write(t, filepath.Join(golden, "access-token/manifest.json"), `{"set":"access-token","tag":"`+tag+`"}`)
	write(t, filepath.Join(golden, "access-token/vectors/valid.json"), `{"v":"`+tag+`"}`)
	write(t, filepath.Join(golden, "workload-token/manifest.json"), `{"set":"workload-token","tag":"`+tag+`"}`)
	write(t, filepath.Join(golden, "workload-token/vectors/valid.json"), `{"w":"`+tag+`"}`)
	write(t, filepath.Join(examples, "discovery/valid.json"), `{"d":"`+tag+`"}`)
	write(t, filepath.Join(examples, "jwks/valid.json"), `{"j":"`+tag+`"}`)
	for _, set := range pepExampleSets {
		write(t, filepath.Join(examples, set, "valid.json"), `{"`+set+`":"`+tag+`"}`)
	}
	write(t, filepath.Join(f.src, "contracts/auth/v1/jsonschema/authorization_decision.v1.schema.json"), `{"s":"`+tag+`"}`)
	write(t, filepath.Join(f.src, "contracts/auth/v1/jsonschema/unrelated.json"), `{"u":"`+tag+`"}`)
	write(t, filepath.Join(f.src, "contracts/auth/v1/vocabulary/reason-codes.json"), `{"r":"`+tag+`"}`)
	write(t, filepath.Join(f.src, "contracts/auth/v1/vocabulary/actions.v1.json"), `{"a":"`+tag+`"}`)
	write(t, filepath.Join(f.src, "contracts/auth/v1/endpoint-profile.schema.json"), `{"e":"`+tag+`"}`)
	run(t, f.src, "git", "add", "-A")
	run(t, f.src, "git", "commit", "-q", "-m", tag)
	run(t, f.src, "git", "push", "-q", "origin", "main")
	return run(t, f.src, "git", "rev-parse", "HEAD")
}

// target is a copy of the files the script edits, with the real test files so
// the constants have their real shape.
func newTarget(t *testing.T, base string) string {
	t.Helper()
	target := filepath.Join(base, "go")
	for _, rel := range []string{"scripts/sync-corpus.sh", accessTokenTest, workloadTest, pepTest} {
		write(t, filepath.Join(target, rel), read(t, filepath.Join("..", rel)))
	}
	write(t, filepath.Join(target, "authverify/testdata/jwks/stale.json"), "stale")
	write(t, filepath.Join(target, "pep/testdata/authorization_decision/stale.json"), "stale")
	return target
}

func syncScript(target, ops string) *exec.Cmd {
	return exec.Command("bash", filepath.Join(target, "scripts/sync-corpus.sh"), ops)
}

func constantLine(name, sha string) string {
	return "const " + name + ` = "` + sha + `"`
}

func withConstant(text, name, sha string) string {
	re := regexp.MustCompile(`(?m)^const ` + name + ` = "[0-9a-f]{40}"$`)
	return re.ReplaceAllLiteralString(text, constantLine(name, sha))
}

func sumRow(t *testing.T, root, name string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(read(t, filepath.Join(root, name))))
	return hex.EncodeToString(sum[:]) + "  " + name
}

func checkPins(t *testing.T, target, sha string, originalAccess, originalWorkload, originalPep string) {
	t.Helper()
	if got, want := read(t, filepath.Join(target, accessTokenTest)), withConstant(originalAccess, accessConst, sha); got != want {
		t.Errorf("%s: only the %s line may change, to %s", accessTokenTest, accessConst, sha)
	}
	if got, want := read(t, filepath.Join(target, workloadTest)), withConstant(originalWorkload, workloadConst, sha); got != want {
		t.Errorf("%s: only the %s line may change, to %s", workloadTest, workloadConst, sha)
	}
	if got, want := read(t, filepath.Join(target, pepTest)), withConstant(originalPep, pepConst, sha); got != want {
		t.Errorf("%s: only the %s line may change, to %s", pepTest, pepConst, sha)
	}
	checkPepSource(t, target, sha)
	data := filepath.Join(target, "authverify/testdata")
	source := read(t, filepath.Join(data, "SOURCE"))
	for _, want := range []string{
		"ops_commit " + sha + "\nops_path contracts/auth/v1/golden/access-token\nops_path contracts/auth/v1/examples/discovery\nops_path contracts/auth/v1/examples/jwks\n",
		sumRow(t, data, "access-token/manifest.json") + "\n",
		sumRow(t, data, "discovery/valid.json") + "\n",
		sumRow(t, data, "jwks/valid.json") + "\n",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("SOURCE lacks %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "workload-token") || strings.Contains(source, "stale.json") {
		t.Errorf("SOURCE names a workload or stale file:\n%s", source)
	}
	workload := read(t, filepath.Join(data, "workload-token.SOURCE"))
	for _, want := range []string{
		"ops_commit " + sha + "\nops_path contracts/auth/v1/golden/workload-token\n",
		sumRow(t, data, "workload-token/manifest.json") + "\n",
		sumRow(t, data, "workload-token/vectors/valid.json") + "\n",
	} {
		if !strings.Contains(workload, want) {
			t.Errorf("workload-token.SOURCE lacks %q:\n%s", want, workload)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "jwks/stale.json")); err == nil {
		t.Error("a stale file survived the sync")
	}
}

func checkPepSource(t *testing.T, target, sha string) {
	t.Helper()
	data := filepath.Join(target, "pep/testdata")
	source := read(t, filepath.Join(data, "SOURCE"))
	head := "ops_commit " + sha + "\n"
	for _, set := range pepExampleSets {
		head += "ops_path contracts/auth/v1/examples/" + set + "\n"
	}
	head += "ops_path contracts/auth/v1/jsonschema\nops_path contracts/auth/v1/vocabulary/reason-codes.json\nops_path contracts/auth/v1/endpoint-profile.schema.json\n"
	if !strings.HasPrefix(source, head) {
		t.Errorf("pep SOURCE head is not %q:\n%s", head, source)
	}
	wants := []string{
		sumRow(t, data, "jsonschema/authorization_decision.v1.schema.json") + "\n",
		sumRow(t, data, "jsonschema/unrelated.json") + "\n",
		sumRow(t, data, "vocabulary/reason-codes.json") + "\n",
		sumRow(t, data, "endpoint/endpoint-profile.schema.json") + "\n",
	}
	for _, set := range pepExampleSets {
		wants = append(wants, sumRow(t, data, set+"/valid.json")+"\n")
	}
	for _, want := range wants {
		if !strings.Contains(source, want) {
			t.Errorf("pep SOURCE lacks %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "actions.v1.json") || strings.Contains(source, "stale.json") || strings.Contains(source, "access-token") {
		t.Errorf("pep SOURCE names a file outside its sets:\n%s", source)
	}
	if _, err := os.Stat(filepath.Join(data, "authorization_decision/stale.json")); err == nil {
		t.Error("a stale pep file survived the sync")
	}
	if _, err := os.Stat(filepath.Join(data, "vocabulary/actions.v1.json")); err == nil {
		t.Error("the vocabulary directory was copied whole")
	}
}

func TestSyncCorpusMovesEveryPinTogether(t *testing.T) {
	base := t.TempDir()
	ops := newOps(t, base)
	target := newTarget(t, base)
	originalAccess := read(t, filepath.Join(target, accessTokenTest))
	originalWorkload := read(t, filepath.Join(target, workloadTest))
	originalPep := read(t, filepath.Join(target, pepTest))

	first := run(t, ops.src, "git", "rev-parse", "HEAD")
	if out, err := syncScript(target, ops.clone).CombinedOutput(); err != nil {
		t.Fatalf("first sync: %v\n%s", err, out)
	}
	checkPins(t, target, first, originalAccess, originalWorkload, originalPep)

	second := ops.commit(t, "two")
	if out, err := syncScript(target, ops.clone).CombinedOutput(); err != nil {
		t.Fatalf("second sync: %v\n%s", err, out)
	}
	checkPins(t, target, second, originalAccess, originalWorkload, originalPep)

	if out, err := syncScript(target, ops.clone).CombinedOutput(); err != nil {
		t.Fatalf("repeat sync: %v\n%s", err, out)
	}
	checkPins(t, target, second, originalAccess, originalWorkload, originalPep)
}

func TestSyncCorpusRefusesBeforeWritingWhenAPinConstantIsMissing(t *testing.T) {
	for _, tc := range []struct{ name, file, constant string }{
		{"access-token", accessTokenTest, accessConst},
		{"workload", workloadTest, workloadConst},
		{"pep", pepTest, pepConst},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			ops := newOps(t, base)
			target := newTarget(t, base)
			path := filepath.Join(target, tc.file)
			write(t, path, strings.Replace(read(t, path), "const "+tc.constant, "const renamed"+tc.constant, 1))
			out, err := syncScript(target, ops.clone).CombinedOutput()
			if err == nil {
				t.Fatalf("the sync succeeded without the %s constant:\n%s", tc.constant, out)
			}
			if !strings.Contains(string(out), tc.constant) {
				t.Errorf("the refusal does not name %s:\n%s", tc.constant, out)
			}
			data := filepath.Join(target, "authverify/testdata")
			for _, name := range []string{"SOURCE", "workload-token.SOURCE", "access-token", "workload-token"} {
				if _, statErr := os.Stat(filepath.Join(data, name)); statErr == nil {
					t.Errorf("%s was written before the refusal", name)
				}
			}
			if _, statErr := os.Stat(filepath.Join(target, "pep/testdata/SOURCE")); statErr == nil {
				t.Error("pep/testdata/SOURCE was written before the refusal")
			}
			if _, statErr := os.Stat(filepath.Join(target, "pep/testdata/authorization_decision/stale.json")); statErr != nil {
				t.Error("the pep corpus was touched before the refusal")
			}
			if _, statErr := os.Stat(filepath.Join(data, "jwks/stale.json")); statErr != nil {
				t.Error("the corpus was touched before the refusal")
			}
		})
	}
}
