package readers

import (
	"context"
	"log/slog"
	"time"
)

// SlogInstrumentation is a ready-made Instrumentation adapter for a consumer
// whose telemetry idiom is structured log/slog records rather than a live
// OTel meter/tracer pipeline (acr is the first such consumer -- it imports
// go.opentelemetry.io/otel only to suppress Genkit's own export, and has no
// configured OTel exporter anywhere; see the README's "Boundary corrections"
// section). It emits exactly one slog record per QueryOrgScoped call, at
// query completion, so it cannot silently discard a call the way a
// misconfigured or unwired OTel provider would.
type SlogInstrumentation struct {
	logger *slog.Logger
	level  slog.Level
}

// NewSlogInstrumentation builds a SlogInstrumentation that logs through
// logger at level (slog.LevelInfo is a reasonable default for a caller that
// does not otherwise care). A nil logger falls back to slog.Default().
func NewSlogInstrumentation(logger *slog.Logger, level slog.Level) *SlogInstrumentation {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogInstrumentation{logger: logger, level: level}
}

// StartQuery implements Instrumentation.
func (s *SlogInstrumentation) StartQuery(ctx context.Context, reader string, orgScoped bool) (context.Context, func(error)) {
	started := time.Now()
	return ctx, func(err error) {
		durationMS := time.Since(started).Milliseconds()
		attrs := []any{
			slog.String("reader", reader),
			slog.Bool("org_scoped", orgScoped),
			slog.Int64("duration_ms", durationMS),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
			s.logger.LogAttrs(ctx, s.level, "readers.query_org_scoped", slogAttrs(attrs)...)
			return
		}
		s.logger.LogAttrs(ctx, s.level, "readers.query_org_scoped", slogAttrs(attrs)...)
	}
}

// slogAttrs converts a []any of slog.Attr values (as built above) into
// []slog.Attr for LogAttrs. Kept as a tiny helper so StartQuery's closure
// reads as a flat attribute list rather than a manual type-assertion loop.
func slogAttrs(values []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(values))
	for _, v := range values {
		if attr, ok := v.(slog.Attr); ok {
			out = append(out, attr)
		}
	}
	return out
}

var _ Instrumentation = (*SlogInstrumentation)(nil)
