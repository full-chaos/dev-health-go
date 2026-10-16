# llm oracles

Each `*.oracle.json` file holds what a reference implementation sent and
returned for a fixed case table. The Go tests replay the same cases through
this package and compare. The header names the producer and the commit it ran
at. Each file is pinned by its sha256 in its test, so a changed byte means a
new recording, never a hand edit.

## openai_sync.oracle.json

- Producer: dev-health-ops `internal/jobs/investment/categorize`
  `OpenAIProvider.Complete`, the ops Go port, at the header's `ops_sha`.
- Generator: `ops_openai_sync_generator.go.txt`. It is kept as text so this
  module never compiles it.
- Test: `oracle_openai_test.go`. It checks byte-equal request bodies, call
  count, text, model, usage, error class and Retry-After.

To record it again, use a throwaway ops worktree at the chosen commit. Never
use a shared checkout, and never commit the generator to ops.

```bash
git -C <ops> worktree add --detach /tmp/ops-oracle <sha>
cp ops_openai_sync_generator.go.txt \
  /tmp/ops-oracle/internal/jobs/investment/categorize/zz_llm_oracle_gen_test.go
cd /tmp/ops-oracle
LLM_ORACLE_OUT=$PWD/openai_sync.oracle.json LLM_ORACLE_OPS_SHA=<sha> \
  go test -count=1 -run TestZZGenerateLLMOracle ./internal/jobs/investment/categorize/
```

## openai_batch.oracle.json

- Corpus: ops' frozen-Python batch corpus (`batch_frozen_python_test.go`).
- Answers recorded: the frozen Python answer, read from ops
  `testdata/golden/batch.golden.json` (recorded from openai.py at
  `python_build`; its sha256 is in the header), and the ops Go port's
  `batchLine`, `parseOpenAIBatchLines` and `mapOpenAIBatchStatus` at
  `ops_sha`.
- Generator: `ops_openai_batch_generator.go.txt`.
- Test: `oracle_batch_test.go`. This is a no-regression guard for the ops
  adoption, not a Python-parity claim. It checks llm against the ops port byte
  for byte on lines, and field for field on parse and status, which are
  caller-relied. The Python answer is compared as well, with ops' six named
  divergences. A deliberate improvement (`cancelling` is not terminal) is
  named in the test.

To record it again, follow the sync recipe above with this generator, test
`TestZZGenerateLLMBatchOracle` and variable `LLM_BATCH_ORACLE_OUT`.
