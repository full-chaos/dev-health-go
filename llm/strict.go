package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// CompactJSON is llm/json_utils.py's validate_json_or_empty: the text as
// compact JSON, or "" when it is blank or not one JSON value.
func CompactJSON(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return ""
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(compact)
}

// Outcome is how CompleteStrict ended.
type Outcome string

const (
	// OutcomeOK: the first answer passed validate.
	OutcomeOK Outcome = "ok"
	// OutcomeRepaired: the first answer failed, the repair answer passed.
	OutcomeRepaired Outcome = "repaired"
	// OutcomeInvalid: both answers failed; the caller applies its
	// deterministic fallback.
	OutcomeInvalid Outcome = "invalid"
)

// ErrEmptyAnswer is the validation failure of an answer with no JSON at all.
var ErrEmptyAnswer = errors.New("llm: the answer is not JSON")

// CompleteStrict is the compute-time contract of the categorization and
// explanation callers: one call, the caller's validation, and at most one
// repair call built by repair from the rejected text and the reason; then the
// caller's deterministic fallback when the outcome is OutcomeInvalid. calls
// holds every Result in order (one or two), for token accounting: a repaired
// answer cost both calls.
//
// validate receives Result.Text (compact JSON under a Format, "" when the
// provider returned no JSON, which fails with ErrEmptyAnswer before validate
// is asked). A provider error ends the call with that error and no outcome.
func CompleteStrict(
	ctx context.Context, provider Provider, request Request,
	validate func(text string) error, repair func(rejected string, reason error) Request,
) (final Result, outcome Outcome, calls []Result, err error) {
	check := func(result Result) error {
		if result.Text == "" {
			return ErrEmptyAnswer
		}
		return validate(result.Text)
	}
	first, err := provider.Complete(ctx, request)
	if err != nil {
		return Result{}, "", nil, err
	}
	calls = append(calls, first)
	reason := check(first)
	if reason == nil {
		return first, OutcomeOK, calls, nil
	}
	second, err := provider.Complete(ctx, repair(first.Text, reason))
	if err != nil {
		return first, "", calls, err
	}
	calls = append(calls, second)
	if check(second) == nil {
		return second, OutcomeRepaired, calls, nil
	}
	return second, OutcomeInvalid, calls, nil
}
