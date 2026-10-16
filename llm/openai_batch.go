package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

// The OpenAI Batch API (openai.py's submit_batch/poll_batch/
// fetch_batch_results/cancel_batch), ported from ops
// internal/jobs/investment/categorize/openaibatch.go (CHAOS-8720). Every call
// has its own span with llm.batch.operation, and is retried once on a
// retryable failure like the synchronous call -- except the create, which is
// billed and not idempotent.
const (
	// openAIBatchEndpoint is the Responses API for every line, because the
	// synchronous call of this provider is.
	openAIBatchEndpoint         = "/v1/responses"
	openAIBatchCompletionWindow = "24h"
	openAIBatchDefaultFileName  = "batch.jsonl"

	batchOpUpload   = "upload"
	batchOpCreate   = "create"
	batchOpRetrieve = "retrieve"
	batchOpContent  = "content"
	batchOpCancel   = "cancel"
)

var _ BatchProvider = (*OpenAI)(nil)

type openAIBatchLine struct {
	CustomID string                 `json:"custom_id"`
	Method   string                 `json:"method"`
	URL      string                 `json:"url"`
	Body     openAIResponsesRequest `json:"body"`
}

// SubmitBatch uploads one JSONL file (purpose batch) and creates the batch.
// Each line's body is the synchronous request body, from the same builder,
// at the first attempt's output cap.
func (p *OpenAI) SubmitBatch(ctx context.Context, batch BatchRequest) (BatchSubmission, error) {
	if err := validateBatchItems(batch.Items); err != nil {
		return BatchSubmission{}, err
	}
	var payload bytes.Buffer
	for index, item := range batch.Items {
		line, err := json.Marshal(openAIBatchLine{
			CustomID: item.CustomID,
			Method:   http.MethodPost,
			URL:      openAIBatchEndpoint,
			Body:     p.responsesBody(item.Request, max(p.cfg.MaxOutputTokens, item.Request.MaxOutputTokens)),
		})
		if err != nil {
			return BatchSubmission{}, fmt.Errorf("llm: encode batch line: %w", err)
		}
		if index > 0 {
			payload.WriteByte('\n')
		}
		payload.Write(line)
	}

	// Admitted only once the batch is known to be sendable, so a budget is
	// never asked to admit a batch that then fails before any upload.
	if err := admitBatch(ctx, p.cfg.Budget, KindOpenAI, p.cfg.Model, p.cfg.BaseURL); err != nil {
		return BatchSubmission{}, err
	}
	fileName := batch.FileName
	if fileName == "" {
		fileName = openAIBatchDefaultFileName
	}
	upload, contentType, err := multipartUpload(payload.Bytes(), fileName)
	if err != nil {
		return BatchSubmission{}, err
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	if err := p.batchCall(ctx, batchOpUpload, http.MethodPost, "/files", contentType, upload, decodeInto(&uploaded)); err != nil {
		return BatchSubmission{}, err
	}
	if uploaded.ID == "" {
		return BatchSubmission{}, NewError(ClassOutput, KindOpenAI, p.cfg.Model, "LLM provider batch file upload returned no file id.")
	}

	create := map[string]any{
		"input_file_id":     uploaded.ID,
		"endpoint":          openAIBatchEndpoint,
		"completion_window": openAIBatchCompletionWindow,
	}
	if len(batch.Metadata) > 0 {
		create["metadata"] = batch.Metadata
	}
	createBody, err := json.Marshal(create)
	if err != nil {
		return BatchSubmission{}, fmt.Errorf("llm: encode batch create: %w", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := p.batchCall(ctx, batchOpCreate, http.MethodPost, "/batches", "application/json", createBody, decodeInto(&created)); err != nil {
		return BatchSubmission{}, err
	}
	if created.ID == "" {
		return BatchSubmission{}, NewError(ClassOutput, KindOpenAI, p.cfg.Model, "LLM provider batch create returned no batch id.")
	}
	return BatchSubmission{ProviderJobID: created.ID, InputFileID: uploaded.ID, ItemCount: len(batch.Items)}, nil
}

type openAIBatchObject struct {
	Status        string `json:"status"`
	OutputFileID  string `json:"output_file_id"`
	ErrorFileID   string `json:"error_file_id"`
	RequestCounts *struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	} `json:"request_counts"`
}

func (p *OpenAI) retrieveBatch(ctx context.Context, providerJobID string) (openAIBatchObject, error) {
	var batch openAIBatchObject
	err := p.batchCall(ctx, batchOpRetrieve, http.MethodGet, "/batches/"+url.PathEscape(providerJobID), "", nil, decodeInto(&batch))
	return batch, err
}

// PollBatch is openai.py's poll_batch.
func (p *OpenAI) PollBatch(ctx context.Context, providerJobID string) (BatchState, error) {
	batch, err := p.retrieveBatch(ctx, providerJobID)
	if err != nil {
		return BatchState{}, err
	}
	state := BatchState{
		ProviderJobID:  providerJobID,
		Status:         mapOpenAIBatchStatus(batch.Status),
		ProviderStatus: batch.Status,
		OutputFileID:   batch.OutputFileID,
		ErrorFileID:    batch.ErrorFileID,
	}
	if counts := batch.RequestCounts; counts != nil {
		state.TotalCount, state.CompletedCount, state.FailedCount = counts.Total, counts.Completed, counts.Failed
	}
	return state, nil
}

// FetchBatchResults is openai.py's fetch_batch_results: the output file's
// lines, then the error file's.
func (p *OpenAI) FetchBatchResults(ctx context.Context, providerJobID string) ([]BatchItemResult, error) {
	batch, err := p.retrieveBatch(ctx, providerJobID)
	if err != nil {
		return nil, err
	}
	var results []BatchItemResult
	for _, fileID := range []string{batch.OutputFileID, batch.ErrorFileID} {
		if fileID == "" {
			continue
		}
		var content []byte
		if err := p.batchCall(ctx, batchOpContent, http.MethodGet, "/files/"+url.PathEscape(fileID)+"/content", "", nil,
			func(raw []byte) error { content = raw; return nil }); err != nil {
			return nil, err
		}
		parsed, err := parseOpenAIBatchLines(content)
		if err != nil {
			return nil, err
		}
		results = append(results, parsed...)
	}
	return results, nil
}

// CancelBatch is openai.py's cancel_batch.
func (p *OpenAI) CancelBatch(ctx context.Context, providerJobID string) error {
	return p.batchCall(ctx, batchOpCancel, http.MethodPost, "/batches/"+url.PathEscape(providerJobID)+"/cancel", "", nil,
		func([]byte) error { return nil })
}

func decodeInto(target any) func([]byte) error {
	return func(raw []byte) error { return json.Unmarshal(raw, target) }
}

func multipartUpload(payload []byte, fileName string) ([]byte, string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("purpose", "batch"); err != nil {
		return nil, "", fmt.Errorf("llm: encode upload: %w", err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+strings.ReplaceAll(fileName, `"`, "")+`"`)
	header.Set("Content-Type", "application/jsonl")
	part, err := form.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("llm: encode upload: %w", err)
	}
	if _, err := part.Write(payload); err != nil {
		return nil, "", fmt.Errorf("llm: encode upload: %w", err)
	}
	if err := form.Close(); err != nil {
		return nil, "", fmt.Errorf("llm: encode upload: %w", err)
	}
	return body.Bytes(), form.FormDataContentType(), nil
}

// batchCall sends one Batch API request, retried once on a retryable failure
// (never the create: a retry after a lost answer would start a second batch
// and orphan the first; openai.py sent it once, max_retries=0). A failure is
// the classified *Error the synchronous call returns.
func (p *OpenAI) batchCall(ctx context.Context, operation, method, path, contentType string, body []byte, decode func([]byte) error) error {
	ctx = withBatchOperation(ctx, operation)
	retries := openAIMaxRetries
	if operation == batchOpCreate {
		retries = 0
	}
	var lastErr *Error
	for attempt := 0; attempt <= retries; attempt++ {
		header := http.Header{"Authorization": {"Bearer " + p.cfg.APIKey.Reveal()}}
		if contentType != "" {
			header.Set("Content-Type", contentType)
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		err := exchange(withAttempt(ctx, attempt+1), p.client, method, p.cfg.BaseURL+path, header, reader, KindOpenAI, p.cfg.Model, decode)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		classified := classify(err, KindOpenAI, p.cfg.Model)
		lastErr = classified
		if classified.Retryable() && attempt < retries {
			if !sleepFor(ctx, delayFor(classified, attempt)) {
				return ctx.Err()
			}
			continue
		}
		return classified
	}
	return lastErr
}

// mapOpenAIBatchStatus is openai.py's _map_openai_batch_status.
func mapOpenAIBatchStatus(status string) BatchJobStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "validating", "in_progress", "finalizing":
		return BatchJobRunning
	case "completed":
		return BatchJobSucceeded
	case "failed":
		return BatchJobFailed
	case "expired":
		return BatchJobExpired
	case "cancelling":
		return BatchJobCancelling
	case "cancelled":
		return BatchJobCancelled
	}
	return BatchJobSubmitted
}

type openAIBatchOutputLine struct {
	ID       json.RawMessage `json:"id"`
	CustomID json.RawMessage `json:"custom_id"`
	Response json.RawMessage `json:"response"`
	Error    json.RawMessage `json:"error"`
}

type openAIBatchLineResponse struct {
	StatusCode json.RawMessage `json:"status_code"`
	RequestID  json.RawMessage `json:"request_id"`
	Body       json.RawMessage `json:"body"`
}

type openAIBatchLineError struct {
	Code    json.RawMessage `json:"code"`
	Type    json.RawMessage `json:"type"`
	Message json.RawMessage `json:"message"`
}

// parseOpenAIBatchLines is openai.py's _parse_openai_batch_lines. Each
// response body goes through parseOpenAIResponse, the synchronous call's
// parser; the text is `validate_json_or_empty(text) or text`. A line that is
// not a JSON object fails the whole file; a line with neither an error object
// nor a response object is skipped, as in Python.
func parseOpenAIBatchLines(content []byte) ([]BatchItemResult, error) {
	var results []BatchItemResult
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), len(content)+1)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		trimmed := bytes.TrimSpace(scanner.Bytes())
		if len(trimmed) == 0 {
			continue
		}
		var line openAIBatchOutputLine
		if trimmed[0] != '{' || json.Unmarshal(trimmed, &line) != nil {
			return nil, fmt.Errorf("llm: batch result line %d is not a JSON object", lineNumber)
		}
		result := BatchItemResult{CustomID: jsonScalarText(line.CustomID), LineID: jsonScalarText(line.ID)}

		if lineErr, ok := decodeObject[openAIBatchLineError](line.Error); ok {
			code, errType := jsonScalarText(lineErr.Code), jsonScalarText(lineErr.Type)
			result.ProviderErrorCode, result.ProviderErrorType = code, errType
			result.ErrorCode = firstNonEmpty(code, errType, "error")
			result.ErrorMessage = firstNonEmpty(jsonScalarText(lineErr.Message), result.ErrorCode)
			results = append(results, result)
			continue
		}
		response, ok := decodeObject[openAIBatchLineResponse](line.Response)
		if !ok {
			continue
		}
		result.RequestID = jsonScalarText(response.RequestID)
		result.StatusCode = jsonInt(response.StatusCode)
		var bodyErr error
		if len(bytes.TrimSpace(response.Body)) > 0 {
			parsed, err := parseOpenAIResponse(response.Body)
			bodyErr = err
			if err == nil {
				result.Usage = parsed.usage
				result.Text = firstNonEmpty(CompactJSON(parsed.text), parsed.text)
			}
		}
		if result.Text == "" {
			status := "unknown"
			if result.StatusCode != nil && *result.StatusCode != 0 {
				status = strconv.Itoa(*result.StatusCode)
			}
			result.ErrorCode = "http_" + status
			result.ErrorMessage = "Batch response did not contain completion text"
			if bodyErr != nil {
				result.ErrorMessage += " (the body is not a Responses API body: " + decodeFailure(bodyErr).Error() + ")"
			}
		}
		results = append(results, result)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("llm: batch result file: %w", err)
	}
	return results, nil
}

func decodeObject[T any](raw json.RawMessage) (T, bool) {
	var zero T
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return zero, false
	}
	var value T
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return zero, false
	}
	return value, true
}

// jsonScalarText is the text of a JSON string or number, "" otherwise.
func jsonScalarText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			return text
		}
		return ""
	}
	var number json.Number
	if json.Unmarshal(trimmed, &number) == nil {
		return number.String()
	}
	return ""
}

func jsonInt(raw json.RawMessage) *int {
	var value int
	if json.Unmarshal(bytes.TrimSpace(raw), &value) != nil {
		return nil
	}
	return &value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
