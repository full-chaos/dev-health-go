package llm

import (
	"context"
	"errors"
)

// Budget admits and settles provider calls against a caller's own budget store
// (ops: llmbudget.Guard, the port of llm/budget.py's guard_byo_call). The
// package calls Run once per HTTP attempt, so a retry is admitted as its own
// billable call; invoke sends the attempt and returns the usage the provider
// reported.
//
// A Run that returns an error without calling invoke refused the call: it was
// never sent, and the provider does not retry it. A Run whose error is not the
// error invoke returned (a settle failure after a sent call) is also final,
// since retrying would admit again.
type Budget interface {
	Run(ctx context.Context, attempt Attempt, invoke func(context.Context) (Usage, error)) error
}

// Attempt is one HTTP attempt as the budget sees it.
type Attempt struct {
	// IdempotencyKey is stable for the attempt and unique per attempt.
	IdempotencyKey string
	Provider       Kind
	Model          string
	BaseURL        string
	// MaxInputBytes is the serialized request body's length, an upper bound
	// of the input tokens the provider can bill.
	MaxInputBytes int64
	// MaxOutputTokens is the request's output cap.
	MaxOutputTokens int64
}

// errBudgetRefused marks an error the budget returned on its own.
type budgetError struct{ cause error }

func (e *budgetError) Error() string { return e.cause.Error() }
func (e *budgetError) Unwrap() error { return e.cause }

// IsBudgetRefusal reports whether err is the budget's own answer (it refused,
// or failed to account) rather than a provider failure.
func IsBudgetRefusal(err error) bool {
	var refusal *budgetError
	return errors.As(err, &refusal)
}

// guarded runs send under budget, when there is one. A budget's own error
// comes back wrapped so the caller can tell it from the provider's.
func guarded(ctx context.Context, budget Budget, attempt Attempt, send func(context.Context) (Usage, error)) error {
	if budget == nil {
		_, err := send(ctx)
		return err
	}
	var (
		invoked   bool
		invokeErr error
	)
	err := budget.Run(ctx, attempt, func(ctx context.Context) (Usage, error) {
		invoked = true
		usage, err := send(ctx)
		invokeErr = err
		return usage, err
	})
	switch {
	case err == nil:
		return nil
	case invoked && invokeErr != nil && err == invokeErr:
		return err
	}
	// Refused before sending, or failed to account after: the budget's own.
	return &budgetError{cause: err}
}

func usageOf(input, output, cached *int) Usage {
	return Usage{InputTokens: input, OutputTokens: output, CachedInputTokens: cached}
}
