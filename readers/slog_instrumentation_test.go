package readers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// recordingHandler is a minimal slog.Handler that captures every record
// passed to it, so a test can assert exactly one record was emitted per
// QueryOrgScoped call -- the property that distinguishes this adapter from a
// silently-discarding one.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func attrsOf(r slog.Record) map[string]slog.Value {
	out := make(map[string]slog.Value, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value
		return true
	})
	return out
}

func TestSlogInstrumentation_EmitsOneRecordPerQuery(t *testing.T) {
	handler := &recordingHandler{}
	instr := NewSlogInstrumentation(slog.New(handler), slog.LevelInfo)

	ctx, finish := instr.StartQuery(context.Background(), "ReadRunStatus", true)
	finish(nil)

	records := handler.snapshot()
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 record for a successful query, got %d", len(records))
	}
	attrs := attrsOf(records[0])
	if got := attrs["reader"].String(); got != "ReadRunStatus" {
		t.Errorf("reader attr = %q, want ReadRunStatus", got)
	}
	if got := attrs["org_scoped"].Bool(); !got {
		t.Errorf("org_scoped attr = %v, want true", got)
	}
	if _, ok := attrs["error"]; ok {
		t.Errorf("success record must not carry an error attr, got %v", attrs["error"])
	}
	_ = ctx
}

func TestSlogInstrumentation_EmitsErrorRecordOnFailure(t *testing.T) {
	handler := &recordingHandler{}
	instr := NewSlogInstrumentation(slog.New(handler), slog.LevelInfo)

	_, finish := instr.StartQuery(context.Background(), "ReadPullRequestFacts", false)
	finish(errors.New("boom"))

	records := handler.snapshot()
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 record for a failed query, got %d", len(records))
	}
	attrs := attrsOf(records[0])
	if got := attrs["error"].String(); got != "boom" {
		t.Errorf("error attr = %q, want boom", got)
	}
	if got := attrs["org_scoped"].Bool(); got {
		t.Errorf("org_scoped attr = %v, want false", got)
	}
}

func TestSlogInstrumentation_NilLoggerFallsBackToDefault(t *testing.T) {
	instr := NewSlogInstrumentation(nil, slog.LevelInfo)
	if instr.logger == nil {
		t.Fatal("expected NewSlogInstrumentation(nil, ...) to fall back to a non-nil logger")
	}
	// Must not panic when actually used.
	_, finish := instr.StartQuery(context.Background(), "ReadDeploymentFacts", true)
	finish(nil)
}

var _ Instrumentation = (*SlogInstrumentation)(nil)
