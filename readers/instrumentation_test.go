package readers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/full-chaos/dev-health-go/readers"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// recordedQuery is one StartQuery call a recordingInstrumentation observed,
// plus the error its finish func was eventually called with.
type recordedQuery struct {
	reader    string
	orgScoped bool
	err       error
	finished  bool
}

// recordingInstrumentation is a fake readers.Instrumentation that records
// every StartQuery call and the error its finish func receives, so a test
// can assert QueryOrgScoped actually drives the hook with the attributes
// the store-level instrumentation contract promises.
type recordingInstrumentation struct {
	queries []*recordedQuery
}

func (r *recordingInstrumentation) StartQuery(ctx context.Context, reader string, orgScoped bool) (context.Context, func(error)) {
	rec := &recordedQuery{reader: reader, orgScoped: orgScoped}
	r.queries = append(r.queries, rec)
	return ctx, func(err error) {
		rec.err = err
		rec.finished = true
	}
}

func TestNoopInstrumentationDoesNotPanicOrChangeBehavior(t *testing.T) {
	t.Parallel()

	// Calling the hook directly must be safe and inert.
	ctx, finish := (readers.NoopInstrumentation{}).StartQuery(context.Background(), "ReadRunStatus", true)
	if ctx == nil {
		t.Fatalf("StartQuery() returned nil context")
	}
	finish(nil)
	finish(errors.New("boom")) // calling finish more than once must not panic either

	// A reader called through a plain context.Background() -- i.e. no
	// Instrumentation ever wired in -- must behave exactly as it did before
	// this hook existed: same result, same bindings, no panic.
	client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", rows: [][]any{{"run-1", "success", "repo-1"}}}}}
	rows, err := readers.ReadRunStatus(context.Background(), client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{})
	if err != nil {
		t.Fatalf("ReadRunStatus() error = %v", err)
	}
	if len(rows) != 1 || rows[0].RunID != "run-1" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestContextWithInstrumentationReceivesSpanCounterAttributes(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		rec := &recordingInstrumentation{}
		ctx := readers.ContextWithInstrumentation(context.Background(), rec)
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", rows: [][]any{{"run-1", "success", "repo-1"}}}}}

		if _, err := readers.ReadRunStatus(ctx, client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadRunStatus() error = %v", err)
		}

		if len(rec.queries) != 1 {
			t.Fatalf("queries = %#v, want exactly 1 StartQuery call", rec.queries)
		}
		got := rec.queries[0]
		if got.reader != "ReadRunStatus" {
			t.Fatalf("reader = %q, want %q", got.reader, "ReadRunStatus")
		}
		if !got.orgScoped {
			t.Fatalf("orgScoped = false, want true")
		}
		if !got.finished {
			t.Fatalf("finish was never called")
		}
		if got.err != nil {
			t.Fatalf("finish err = %v, want nil on success", got.err)
		}
	})

	t.Run("error is reported to finish", func(t *testing.T) {
		t.Parallel()
		rec := &recordingInstrumentation{}
		ctx := readers.ContextWithInstrumentation(context.Background(), rec)
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", err: errors.New("boom")}}}

		if _, err := readers.ReadRunStatus(ctx, client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{}); err == nil {
			t.Fatalf("ReadRunStatus() error = nil, want an error")
		}

		if len(rec.queries) != 1 {
			t.Fatalf("queries = %#v, want exactly 1 StartQuery call", rec.queries)
		}
		got := rec.queries[0]
		if got.reader != "ReadRunStatus" {
			t.Fatalf("reader = %q, want %q", got.reader, "ReadRunStatus")
		}
		if !got.finished || got.err == nil {
			t.Fatalf("finish = (%v, %v), want (true, non-nil error)", got.finished, got.err)
		}
	})

	t.Run("distinct readers report their own name", func(t *testing.T) {
		t.Parallel()
		rec := &recordingInstrumentation{}
		ctx := readers.ContextWithInstrumentation(context.Background(), rec)
		client := &fakeClient{tables: []fakeTable{{match: "FROM cicd_metrics_daily", rows: [][]any{cicdMetricsRow("repo-1")}}}}

		if _, err := readers.ReadCICDMetricsDaily(ctx, client, "org-1", []string{"repo-1"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadCICDMetricsDaily() error = %v", err)
		}

		if len(rec.queries) != 1 || rec.queries[0].reader != "ReadCICDMetricsDaily" {
			t.Fatalf("queries = %#v, want exactly 1 call with reader %q", rec.queries, "ReadCICDMetricsDaily")
		}
	})
}

func TestOTelInstrumentationSatisfiesInterface(t *testing.T) {
	t.Parallel()
	var _ readers.Instrumentation = (*readers.OTelInstrumentation)(nil)
}

// TestOTelInstrumentationWiresIntoQueryOrgScoped exercises the ready-made
// OTel adapter end-to-end -- built from real (no-op) OTel providers, the
// same shape acr's own already-configured providers take -- through an
// actual reader call, proving it neither panics nor errors when wired into
// ContextWithInstrumentation for both a successful and a failing query.
func TestOTelInstrumentationWiresIntoQueryOrgScoped(t *testing.T) {
	t.Parallel()

	instr, err := readers.NewOTelInstrumentation(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())
	if err != nil {
		t.Fatalf("NewOTelInstrumentation() error = %v", err)
	}
	ctx := readers.ContextWithInstrumentation(context.Background(), instr)

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", rows: [][]any{{"run-1", "success", "repo-1"}}}}}
		if _, err := readers.ReadRunStatus(ctx, client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{}); err != nil {
			t.Fatalf("ReadRunStatus() error = %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		client := &fakeClient{tables: []fakeTable{{match: "FROM ci_pipeline_runs", err: errors.New("boom")}}}
		if _, err := readers.ReadRunStatus(ctx, client, "org-1", []string{"repo-1:run-1"}, readers.TimeBound{}); err == nil {
			t.Fatalf("ReadRunStatus() error = nil, want an error")
		}
	})
}
