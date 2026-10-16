package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// BatchJobStatus is llm/providers/batch.py's BatchJobStatus, narrowed to the
// values a provider reports.
type BatchJobStatus string

const (
	BatchJobSubmitted BatchJobStatus = "submitted"
	BatchJobRunning   BatchJobStatus = "running"
	// BatchJobCancelling is a cancel the provider has not finished: results
	// can still arrive, so it is not terminal (openai.py folded it into
	// cancelled, which told a caller to stop polling too early).
	BatchJobCancelling BatchJobStatus = "cancelling"
	BatchJobSucceeded  BatchJobStatus = "succeeded"
	BatchJobFailed     BatchJobStatus = "failed"
	BatchJobCancelled  BatchJobStatus = "cancelled"
	BatchJobExpired    BatchJobStatus = "expired"
)

// Terminal reports whether the provider will not change the job any more.
func (s BatchJobStatus) Terminal() bool {
	switch s {
	case BatchJobSucceeded, BatchJobFailed, BatchJobCancelled, BatchJobExpired:
		return true
	}
	return false
}

// BatchItem is one request of a batch; CustomID finds its result again.
type BatchItem struct {
	CustomID string
	Request  Request
}

// BatchRequest is one batch to submit. FileName and Metadata are the
// caller's labels for the uploaded file and the provider job ("" and nil
// leave them generic); ops sends investment-categorization-batch.jsonl and
// {"source": "investment_materialize"}.
type BatchRequest struct {
	Items    []BatchItem
	FileName string
	Metadata map[string]string
}

// BatchSubmission is a provider job that was created.
type BatchSubmission struct {
	ProviderJobID string
	InputFileID   string
	ItemCount     int
}

// BatchState is one poll of a provider job.
type BatchState struct {
	ProviderJobID  string
	Status         BatchJobStatus
	ProviderStatus string
	TotalCount     int
	CompletedCount int
	FailedCount    int
	OutputFileID   string
	ErrorFileID    string
}

// BatchItemResult is one line of a job's output or error file.
type BatchItemResult struct {
	CustomID string
	// Text is the completion text, "" when the line held none.
	Text string
	// ErrorCode is set when the line is not a completion: the provider's
	// error code, or http_<status> for a response with no completion text.
	ErrorCode    string
	ErrorMessage string

	LineID            string
	RequestID         string
	StatusCode        *int
	Usage             Usage
	ProviderErrorCode string
	ProviderErrorType string
}

// Succeeded is batch.py's BatchItemResult.succeeded.
func (r BatchItemResult) Succeeded() bool { return r.ErrorCode == "" && r.Text != "" }

// BatchProvider is a provider's Batch API: many requests at once, at a lower
// price, answered within a completion window. It is optional; AsBatch finds
// it.
type BatchProvider interface {
	SubmitBatch(ctx context.Context, batch BatchRequest) (BatchSubmission, error)
	PollBatch(ctx context.Context, providerJobID string) (BatchState, error)
	// FetchBatchResults returns the output file's lines, then the error
	// file's. A custom id may be missing; the caller decides what that means.
	FetchBatchResults(ctx context.Context, providerJobID string) ([]BatchItemResult, error)
	CancelBatch(ctx context.Context, providerJobID string) error
}

// AsBatch returns the Batch API of provider, if it has one.
func AsBatch(provider Provider) (BatchProvider, bool) {
	batch, ok := provider.(BatchProvider)
	return batch, ok
}

// BatchBudget is a Budget that can decide on a whole batch. A batch is priced
// differently from a synchronous call and its results carry no cached-token
// detail, so a per-attempt reservation cannot be made for it: llm/budget.py
// admitted a batch only for an organization with no budget configured. A
// provider with a Budget that is not a BatchBudget refuses every batch (fail
// closed).
type BatchBudget interface {
	AdmitBatch(ctx context.Context, provider Kind, model, baseURL string) error
}

// ErrEmptyBatch is returned for a batch with no items.
var ErrEmptyBatch = errors.New("llm: cannot submit an empty provider batch")

// ErrBatchUnbudgetable is the refusal of a batch under a Budget that cannot
// decide on one.
var ErrBatchUnbudgetable = errors.New("llm: a batch cannot be admitted by the configured budget")

func admitBatch(ctx context.Context, budget Budget, provider Kind, model, baseURL string) error {
	if budget == nil {
		return nil
	}
	batchBudget, ok := budget.(BatchBudget)
	if !ok {
		return &budgetError{cause: ErrBatchUnbudgetable}
	}
	if err := batchBudget.AdmitBatch(ctx, provider, model, baseURL); err != nil {
		return &budgetError{cause: err}
	}
	return nil
}

// validateBatchItems is BatchItemRequest.__post_init__ over a whole batch,
// plus the rule Python's store kept by a unique key: no custom id twice.
func validateBatchItems(items []BatchItem) error {
	if len(items) == 0 {
		return ErrEmptyBatch
	}
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		if strings.TrimSpace(item.CustomID) == "" {
			return fmt.Errorf("llm: batch item %d: custom id is required", index)
		}
		if strings.IndexFunc(item.CustomID, unicode.IsSpace) >= 0 {
			return fmt.Errorf("llm: batch item %d: custom id must not contain whitespace", index)
		}
		if item.Request.Prompt == "" {
			return fmt.Errorf("llm: batch item %d: prompt is required", index)
		}
		if item.Request.Seed != nil {
			return fmt.Errorf("llm: batch item %d: the batch API takes no seed", index)
		}
		if _, dup := seen[item.CustomID]; dup {
			return fmt.Errorf("llm: batch item %d: custom id %q appears twice", index, item.CustomID)
		}
		seen[item.CustomID] = struct{}{}
	}
	return nil
}

// BatchItemFailure is the error of one batch line that holds no completion,
// classified as the same failure of a synchronous call would be.
func BatchItemFailure(result BatchItemResult, provider Kind, model string) error {
	status := 0
	if result.StatusCode != nil {
		status = *result.StatusCode
	}
	// A 2xx line with no completion text and no provider error is a reply
	// with no usable answer: an output failure, which a caller may retry.
	if status >= 200 && status < 300 && result.ProviderErrorCode == "" && result.ProviderErrorType == "" {
		return NewError(ClassOutput, provider, model, "LLM provider batch line held no completion text.")
	}
	// The line's error type rides along with its code: a code like
	// invalid_prompt says nothing a classifier knows, its type
	// invalid_request_error does.
	return classify(&statusError{statusCode: status, body: strings.Join([]string{
		result.ErrorCode, result.ProviderErrorType, result.ErrorMessage}, " ")}, provider, model)
}

// BatchMissingResultError is the error of a request whose custom id is in no
// line of a finished batch.
func BatchMissingResultError(provider Kind, model string) error {
	return NewError(ClassOutput, provider, model, "LLM provider batch returned no result for the request.")
}

// BatchTimeoutError is the error of each request of a batch that did not
// finish within the caller's timeout.
func BatchTimeoutError(provider Kind, model string) error {
	return NewError(ClassBatchTimeout, provider, model, "LLM provider batch did not finish within the timeout.")
}

// BatchEndedError is the error of each request of a batch that ended failed,
// expired or cancelled at the provider.
func BatchEndedError(status BatchJobStatus, provider Kind, model string) error {
	return NewError(ClassServer, provider, model, "LLM provider batch ended "+string(status)+".")
}
