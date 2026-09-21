package gateway

import (
	"context"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/models"
)

// Tracing of the data plane. Spans are produced only when an OTLP endpoint is
// configured (see internal/tracing); otherwise every call here resolves to the
// no-op tracer and costs a nil check.
//
// SECURITY — what a span may carry. This handler reads the client's raw body
// (the sensitive values themselves), its Authorization header and the model's
// answer. A span leaves the process and lands in a trace store with a
// different audience than the masked traffic, so it carries REQUEST METADATA
// ONLY:
//
//   - allowed: the matched route, the wire dialect, the policy mode, body and
//     text SIZES, counts, and rule / data-type identifiers (the same IDs the
//     Prometheus metrics and the response headers already expose);
//   - never: request or response bytes, extracted values, the placeholders
//     that stand in for them, model names, request headers of any kind, or the
//     request URL (its query string is client-controlled and may carry a
//     credential).
//
// For the same reason the error paths below set a span status with a FIXED
// message instead of recording the error value: an upstream error wraps the
// outbound URL, query string included.
var tracer = otel.Tracer("github.com/cloud-ru-tech/guardrails-llm-filter/internal/controller/gateway")

// Span names. The server span is named after the matched route, the rest are
// stable identifiers of the pipeline phases.
const (
	spanMask      = "guardrails.mask"
	spanUpstream  = "guardrails.upstream"
	spanDemask    = "guardrails.demask"
	spanDemaskSSE = "guardrails.demask.sse"
)

// routeUnguarded stands in for the request path on a path this service does
// not guard. The path is client-controlled and unbounded (suffix matching
// means any prefix reaches here), so it never becomes a span name or value.
const routeUnguarded = "(unguarded)"

// Span attribute keys. The guardrails.* namespace mirrors the vocabulary of
// the Prometheus metrics (internal/metrics) so a trace and a dashboard can be
// read against each other.
const (
	attrRoute          = attribute.Key("guardrails.route")
	attrGuarded        = attribute.Key("guardrails.guarded")
	attrDialect        = attribute.Key("guardrails.dialect")
	attrMode           = attribute.Key("guardrails.mode")
	attrMaskable       = attribute.Key("guardrails.maskable")
	attrStreaming      = attribute.Key("guardrails.streaming")
	attrRequestBytes   = attribute.Key("guardrails.request_body_bytes")
	attrResponseBytes  = attribute.Key("guardrails.response_body_bytes")
	attrDemasked       = attribute.Key("guardrails.demasked")
	attrTexts          = attribute.Key("guardrails.texts")
	attrTextBytes      = attribute.Key("guardrails.text_bytes")
	attrRuleIDs        = attribute.Key("guardrails.rule_ids")
	attrRulesTriggered = attribute.Key("guardrails.rules_triggered")
	attrDataTypes      = attribute.Key("guardrails.data_types")
	attrReplacements   = attribute.Key("guardrails.replacements")
	attrOutcome        = attribute.Key("guardrails.outcome")
	attrFields         = attribute.Key("guardrails.response_fields")
)

// Values of guardrails.outcome on the mask span: why the request was or was
// not masked. Every value but outcomeMasked leaves the body untouched
// (fail-open) and the response relayed verbatim.
const (
	outcomeMasked            = "masked"
	outcomeDetect            = "detect"
	outcomeNoFindings        = "no_findings"
	outcomeNoFields          = "no_fields"
	outcomeUnsupportedSchema = "unsupported_schema"
	outcomeError             = "error"
)

// startRequestSpan opens the server span for one data-plane request, adopting
// the caller's traceparent so this hop continues their trace. The span is
// named after the MATCHED ROUTE (a configured path), never the raw request
// path.
func startRequestSpan(r *http.Request, format models.APIFormat, route string, guarded bool) (context.Context, trace.Span) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	if !guarded {
		route = routeUnguarded
	}
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(r.Method),
		attrRoute.String(route),
		attrGuarded.Bool(guarded),
	}
	if guarded {
		attrs = append(attrs, attrDialect.String(string(format)))
	}

	return tracer.Start(ctx, r.Method+" "+route,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	)
}

// startUpstreamSpan opens the client span covering the call to the LLM provider
// and puts ITS traceparent on the outbound request, replacing the one copied
// from the client, so the provider's own spans hang under this hop instead of
// beside it. The request keeps the client's context, and with it the client's
// cancellation — only the header carries the span.
//
// The span is closed when the response headers arrive, so its duration is the
// upstream's time to first byte, not the length of a stream.
func startUpstreamSpan(ctx context.Context, req *http.Request) trace.Span {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(req.Method),
		semconv.ServerAddress(req.URL.Hostname()),
		semconv.URLScheme(req.URL.Scheme),
	}
	if port, err := strconv.Atoi(req.URL.Port()); err == nil {
		attrs = append(attrs, semconv.ServerPort(port))
	}
	spanCtx, span := tracer.Start(ctx, spanUpstream,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	otel.GetTextMapPropagator().Inject(spanCtx, propagation.HeaderCarrier(req.Header))
	return span
}

// endUpstreamSpan closes the upstream span with the status the provider
// returned.
func endUpstreamSpan(span trace.Span, status int) {
	span.SetAttributes(semconv.HTTPResponseStatusCode(status))
	span.End()
}

// failUpstreamSpan closes the upstream span with a fixed reason. The error
// value is deliberately not recorded — see the security note above.
func failUpstreamSpan(span trace.Span, reason string) {
	span.SetStatus(codes.Error, reason)
	span.End()
}

// recordResponse marks the server span with the status the client receives
// (the upstream's, relayed verbatim) and whether the body was demasked.
func recordResponse(ctx context.Context, status int, demasked bool) {
	trace.SpanFromContext(ctx).SetAttributes(
		semconv.HTTPResponseStatusCode(status),
		attrDemasked.Bool(demasked),
	)
}

// failSpan marks the span in ctx as failed with a fixed reason. The reason is
// a constant from this package, never a formatted error: see the security note
// above.
func failSpan(ctx context.Context, reason string) {
	trace.SpanFromContext(ctx).SetStatus(codes.Error, reason)
}

// setSpanAttributes adds attributes to the span in ctx, if any.
func setSpanAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}

// dataTypeIDs renders triggered data types as their numeric IDs — the same
// identifiers the response headers and metrics carry.
func dataTypeIDs(dataTypes []models.DataType) []string {
	out := make([]string, len(dataTypes))
	for i, dt := range dataTypes {
		out[i] = strconv.Itoa(int(dt))
	}
	return out
}
