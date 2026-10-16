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

## python_oracle/anthropic.oracle.json

- Producer: the frozen Python `AnthropicProvider`, `llm/providers/anthropic.py`,
  executed with its anthropic SDK at the header's `ops_sha`. That is
  `9fb387470e4ab360f88bc052694c891d586824ce`, the build ops' own batch oracle
  uses; its provider files are byte-identical to the last commit that held
  them, and the header lists each file's sha256.
- Driver: `python_oracle/driver.py`. It replaces the provider's hardened HTTP
  client with one on an in-memory transport that records each request and
  serves the case's recorded answer. Nothing reaches the network.
- Test: `oracle_anthropic_test.go`. The Python body is the oracle for the
  request, and the Python result for the parse. Each intentional difference is
  declared there with its reason and must still differ.

To record it again, with no shared checkout touched:

```bash
git -C <ops> worktree add --detach /tmp/ops-py 9fb387470e4ab360f88bc052694c891d586824ce
cd /tmp/ops-py && uv sync --frozen --no-install-project
.venv/bin/python -I <this repo>/llm/testdata/python_oracle/driver.py /tmp/ops-py/src anthropic \
  9fb387470e4ab360f88bc052694c891d586824ce > anthropic.oracle.json
```

`LLM_PYTHON_ORACLE_OPS=/tmp/ops-py go test ./llm -run PythonOracleReExecutes`
runs the driver again and requires byte-equal output.
