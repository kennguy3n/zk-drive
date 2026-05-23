package tracing

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RenameHTTPServerSpan renames the active span on ctx to
// `<method> <route>` and sets the http.route attribute. It is a thin
// wrapper around trace.SpanFromContext + Span.SetName so the call
// site (a chi middleware in cmd/server/main.go) doesn't need to
// import the OTel trace package directly. When the active span is
// the no-op span (because OTEL_EXPORTER_OTLP_ENDPOINT is unset),
// SetName / SetAttributes are silent no-ops, so this is safe to
// call unconditionally.
//
// Cardinality contract: route is the chi route PATTERN
// (e.g. "/api/files/{id}"), not the raw request path. Callers are
// responsible for passing a pattern with bounded cardinality so
// trace backends don't store a unique span name per UUID.
func RenameHTTPServerSpan(ctx context.Context, method, route string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		// No-op span (no recording happening) — skip the alloc.
		// SetName and SetAttributes would be cheap no-ops on the
		// noop span, but the string concatenation below would
		// still allocate. IsRecording is the OTel-blessed gate
		// for this exact optimisation.
		return
	}
	span.SetName(method + " " + route)
	span.SetAttributes(attribute.String("http.route", route))
}

// SetSpanUser attaches enduser.id and enduser.workspace_id
// attributes to the active span, matching the OpenTelemetry
// `enduser.*` semantic conventions. Used by HTTP handlers after
// they resolve the request's authenticated user / workspace, so
// the same trace can be filtered by tenant in the backend.
//
// Empty values are skipped so a partially-authenticated request
// (e.g. JWT decoded but workspace not yet resolved) doesn't pin
// either attribute to the empty string — which dashboards would
// then bucket all unauthenticated traffic against.
func SetSpanUser(ctx context.Context, userID, workspaceID string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 2)
	if userID != "" {
		attrs = append(attrs, attribute.String("enduser.id", userID))
	}
	if workspaceID != "" {
		attrs = append(attrs, attribute.String("enduser.workspace_id", workspaceID))
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}
