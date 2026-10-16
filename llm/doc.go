// Package llm is the one LLM provider implementation that dev-health-ops and
// acr share (CHAOS-8719): a narrow Provider interface, a client per provider
// API, and the rules every caller relied on in the Python providers it ports
// (dev_health_ops/llm/providers, deleted from ops; read it with
// `git show 8b0a0dc82e^:src/dev_health_ops/llm/providers/<file>` in ops).
//
// WHY IT LIVES HERE. This repository is a store/contract layer, and that scope
// is amended for this package by chris's ruling of 2026-10-16 ("ACR will need
// it too, unless we plan to duplicate the llm provider code"): before it, the
// only Go provider code was a private port inside ops, so acr would have had to
// copy it, and two copies of a provider client drift (the same reasoning that
// made this repository exist for ClickHouse readers).
//
// WHAT IT DOES NOT HOLD. No credential store, no environment lookup, no key
// cache: a caller resolves its credential (an org's BYO settings, a platform
// secret) and passes it in Config, so there is one credential store and it is
// the caller's. No budget store either: Config.Budget is a hook the caller
// implements over its own store, called once per HTTP attempt. No prompt, no
// schema and no validation of a payload's meaning: the caller owns those, and
// the deterministic fallback when CompleteStrict reports an invalid answer.
//
// Contract every Provider keeps (providercontract_test.go runs every Kind
// through it):
//
//   - A Request with a Format asks the provider for strict JSON; a reply that
//     is not one JSON value comes back as Result.Text "" and a nil error, so
//     the caller makes its one repair attempt (CompleteStrict) and then falls
//     back.
//     Result.Unparsed then holds the reply as written, so the caller can say
//     why it failed.
//   - Request.Seed is sent only by the Chat Completions client for local and
//     Qwen; the OpenAI (Responses), Anthropic and Gemini clients refuse a
//     seed before any call rather than drop it.
//   - One retry of a transient failure (rate limit, 5xx, timeout, transport),
//     honouring Retry-After up to 60 s; auth and invalid-request failures are
//     final. Every provider failure is an *Error with a closed ErrorClass
//     and a sanitized message; neither Error() nor any unwrapped layer holds
//     provider text. Two other errors come back as they are: a Budget's own
//     (IsBudgetRefusal) and the caller's context cancellation.
//   - Result.Model is the model the request carried, and Model() returns it
//     before any call.
//   - Usage counts are nil when the provider did not report them, never 0.
//   - One OpenTelemetry span per HTTP attempt, named as in ops
//     (dev_health.llm.request, llm.provider, llm.model, llm.attempt,
//     http.response.status_code, error.class).
//   - No redirect is followed and no ambient proxy is used; the API key never
//     appears in an error, a log value or a printed Config.
//
// Structured output by provider (sources read 2026-10-16):
//
//   - OpenAI (Responses API) and Chat Completions (local, Gemini, Qwen): the
//     Format goes as a strict json_schema. These clients check no schema
//     limit before the call. A Chat Completions server that answers 400 is
//     asked once more as text (a second billed call), as local.py did; that
//     retry stays because ops relies on it.
//   - Anthropic (Messages API): the client checks the Format's schema
//     against the documented structured-output limits before the call
//     (https://platform.claude.com/docs/en/build-with-claude/structured-outputs):
//     additionalProperties must be false on every object; at most 24
//     optional and 16 union-typed parameters; supported keywords only (no
//     if/then/else, not, oneOf, numeric or string bounds, array bounds
//     beyond minItems 0 or 1); scalar enums; internal $ref only; known
//     string formats; regex without backreferences, lookaround or word
//     boundaries. A schema that fits goes as output_config.format; one that
//     does not goes as the input_schema of one tool, forced with
//     tool_choice "tool", or "auto" on the models the tool-use docs list as
//     refusing a forced tool (Opus 5.5, Sonnet 5.5, Fable 5.1, Mythos 5.1;
//     https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools).
//     The mode is picked once: a 400 is an invalid_request *Error and is
//     never retried in another mode, because a refused call is billed for
//     its input tokens. Result.FormatPath and Result.FormatReason say which
//     mode was used and why.
//   - Gemini (OpenAI-compatible endpoint) answers 400 to any request that
//     carries seed, so its client refuses Request.Seed before the call.
package llm
