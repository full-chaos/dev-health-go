package readers

import "context"

// Instrumentation is the store-level telemetry hook QueryOrgScoped -- the
// single funnel every reader in this package queries through -- calls on
// every invocation. It carries no reader-specific or consumer-specific
// semantics: a domain name string and a bool are the only inputs, so acr's
// devhealthfacts adapter and a future query-api resolver layer can each
// wire in their own already-configured OTel providers (see
// OTelInstrumentation) without this package knowing either exists.
//
// Wire an implementation in per-request via ContextWithInstrumentation. A
// context that never had one wired in behaves exactly as if
// NoopInstrumentation{} were used, so adding this hook changes no existing
// caller's behavior.
type Instrumentation interface {
	// StartQuery is called once at the start of every QueryOrgScoped call.
	//
	// reader identifies the calling domain reader -- by convention, its
	// exported Go function name (e.g. "ReadRunStatus"). orgScoped is true
	// for every call made through QueryOrgScoped today; it is carried as an
	// explicit attribute rather than assumed so a future non-org-scoped
	// query funnel added to this package can report through the same
	// Instrumentation without a breaking interface change.
	//
	// StartQuery returns the context QueryOrgScoped should continue with
	// (an implementation that starts a span returns the span-carrying
	// context, so the underlying client.Query call it wraps is parented to
	// it) and a finish func. The caller invokes finish exactly once, when
	// the query completes, passing the resulting error (nil on success).
	StartQuery(ctx context.Context, reader string, orgScoped bool) (context.Context, func(err error))
}

// NoopInstrumentation is an Instrumentation that discards everything. Its
// zero value is ready to use. It is also the default instrumentationFromContext
// falls back to when a context carries none.
type NoopInstrumentation struct{}

// StartQuery implements Instrumentation by doing nothing.
func (NoopInstrumentation) StartQuery(ctx context.Context, _ string, _ bool) (context.Context, func(error)) {
	return ctx, func(error) {}
}

var _ Instrumentation = NoopInstrumentation{}

// instrumentationContextKey is unexported so only this package can set or
// read the value ContextWithInstrumentation stores.
type instrumentationContextKey struct{}

// ContextWithInstrumentation returns a copy of ctx that QueryOrgScoped
// reports through instr instead of the default no-op. Passing a nil instr
// is equivalent to not calling this at all.
func ContextWithInstrumentation(ctx context.Context, instr Instrumentation) context.Context {
	if instr == nil {
		return ctx
	}
	return context.WithValue(ctx, instrumentationContextKey{}, instr)
}

// instrumentationFromContext returns the Instrumentation wired into ctx, or
// NoopInstrumentation{} if none was.
func instrumentationFromContext(ctx context.Context) Instrumentation {
	if instr, ok := ctx.Value(instrumentationContextKey{}).(Instrumentation); ok && instr != nil {
		return instr
	}
	return NoopInstrumentation{}
}
