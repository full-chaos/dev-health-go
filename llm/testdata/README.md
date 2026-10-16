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

## anthropic_responses.golden.json

- These are Messages API answers in the documented shape. They are not live
  recordings, because no Anthropic key exists where they were made.
- Each answer is stored with the parsed outcome of the previous Anthropic
  client, which was recorded once: text, model, input and output tokens, and
  error class.
- Test: `oracle_anthropic_test.go`, caller-relied. For the same answer, the
  Result must give the categorize and explain callers what they got before:
  text and JSON, usage (nil means not reported), stop reason, and error class.
  Each place where this client now answers better is named there with its
  reason and must still differ.
- Request shape is not compared. The client follows the current API.

## compatible_responses.golden.json

- These are Chat Completions answers in the documented OpenAI-compatible
  shape. They are not live recordings, because no Gemini, DashScope or local
  key exists where they were made.
- Each answer is stored with the parsed outcome of the previous local, Gemini
  and Qwen clients, which was recorded once.
- Test: `oracle_compatible_test.go`, caller-relied. For the same answer, the
  Result must give the callers what they got before: text and JSON, usage
  (nil means not reported), stop reason, and error class. Request shape is not
  compared.
