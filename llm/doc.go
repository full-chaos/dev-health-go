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
//   - One retry of a transient failure (rate limit, 5xx, timeout, transport),
//     honouring Retry-After up to 60 s; auth and invalid-request failures are
//     final. Every failure is an *Error with a closed ErrorClass and a
//     sanitized message; the provider's raw body never reaches Error().
//   - Result.Model is the model the request carried, and Model() returns it
//     before any call.
//   - Usage counts are nil when the provider did not report them, never 0.
//   - One OpenTelemetry span per HTTP attempt, named as in ops
//     (dev_health.llm.request, llm.provider, llm.model, llm.attempt,
//     http.response.status_code, error.class).
//   - No redirect is followed and no ambient proxy is used; the API key never
//     appears in an error, a log value or a printed Config.
package llm
