package readers

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationScope names the tracer/meter this package's OTel
// instrumentation registers under -- conventionally the instrumenting
// package's import path.
const instrumentationScope = "github.com/full-chaos/dev-health-go/readers"

// OTelInstrumentation is the ready-made Instrumentation for a consumer that
// already has configured OTel providers: acr's devhealthfacts adapter, and
// later a query-api resolver layer, each construct one from their own
// trace.TracerProvider/metric.MeterProvider and wire it in via
// ContextWithInstrumentation. This package never looks up or registers a
// process-wide global provider, so linking it twice into one process (e.g.
// acr importing both this module and its own copy) never causes the two to
// fight over a default.
type OTelInstrumentation struct {
	tracer       trace.Tracer
	queryCounter metric.Int64Counter
	errorCounter metric.Int64Counter
	latency      metric.Float64Histogram
}

// NewOTelInstrumentation builds the tracer and the three instruments
// (a query counter, an error counter, and a latency histogram in
// milliseconds) once, from the given providers.
func NewOTelInstrumentation(tp trace.TracerProvider, mp metric.MeterProvider) (*OTelInstrumentation, error) {
	meter := mp.Meter(instrumentationScope)

	queryCounter, err := meter.Int64Counter(
		"dev_health_go.readers.query_count",
		metric.WithDescription("Number of readers.QueryOrgScoped calls, by reader."),
	)
	if err != nil {
		return nil, err
	}
	errorCounter, err := meter.Int64Counter(
		"dev_health_go.readers.query_error_count",
		metric.WithDescription("Number of readers.QueryOrgScoped calls that returned an error, by reader."),
	)
	if err != nil {
		return nil, err
	}
	latency, err := meter.Float64Histogram(
		"dev_health_go.readers.query_latency_ms",
		metric.WithDescription("readers.QueryOrgScoped call latency, by reader."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, err
	}

	return &OTelInstrumentation{
		tracer:       tp.Tracer(instrumentationScope),
		queryCounter: queryCounter,
		errorCounter: errorCounter,
		latency:      latency,
	}, nil
}

// StartQuery implements Instrumentation.
func (o *OTelInstrumentation) StartQuery(ctx context.Context, reader string, orgScoped bool) (context.Context, func(error)) {
	attrs := attribute.NewSet(
		attribute.String("reader", reader),
		attribute.Bool("org_scoped", orgScoped),
	)
	spanCtx, span := o.tracer.Start(ctx, "readers.QueryOrgScoped", trace.WithAttributes(attrs.ToSlice()...))
	o.queryCounter.Add(spanCtx, 1, metric.WithAttributeSet(attrs))
	start := time.Now()

	return spanCtx, func(err error) {
		o.latency.Record(spanCtx, float64(time.Since(start))/float64(time.Millisecond), metric.WithAttributeSet(attrs))
		if err != nil {
			o.errorCounter.Add(spanCtx, 1, metric.WithAttributeSet(attrs))
			// Deliberately not span.RecordError(err) / err.Error(): err may
			// originate from a domain reader's own row-scan closure, outside
			// this package's control, and its message text is not guaranteed
			// safe to export (see safeErrorClass's doc comment). Only a
			// bounded, closed-vocabulary class is recorded.
			class := safeErrorClass(err)
			span.SetAttributes(attribute.String("error_class", class))
			span.SetStatus(codes.Error, class)
		}
		span.End()
	}
}

var _ Instrumentation = (*OTelInstrumentation)(nil)
