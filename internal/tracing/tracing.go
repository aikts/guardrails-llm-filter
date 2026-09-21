// Package tracing wires the optional OpenTelemetry trace pipeline.
//
// Tracing is OFF unless an OTLP endpoint is configured. Without one no
// exporter and no tracer provider are built, the global provider stays the
// no-op one, and every span start on the request path is a nil check. The W3C
// propagator is installed either way, so an inbound traceparent is extracted
// and re-injected into the upstream request: the caller's trace stays unbroken
// through this hop even with export disabled.
//
// Configuration follows the OpenTelemetry environment specification rather
// than the service's own GUARDRAILS_ prefix, because it is consumed by the SDK
// and its exporters, not by internal/config:
//
//	OTEL_SERVICE_NAME                   service.name (default "guardrails-llm-filter")
//	OTEL_RESOURCE_ATTRIBUTES            extra resource attributes
//	OTEL_EXPORTER_OTLP_ENDPOINT         OTLP endpoint; unset disables tracing
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  the same for traces only; takes precedence
//	OTEL_EXPORTER_OTLP_PROTOCOL         grpc (default) | http/protobuf
//	OTEL_EXPORTER_OTLP_TRACES_PROTOCOL  the same for traces only; takes precedence
//	OTEL_TRACES_SAMPLER                 SDK sampler, e.g. parentbased_traceidratio
//	OTEL_TRACES_SAMPLER_ARG             its argument, e.g. 0.1
//
// The endpoint carries the scheme: an http:// endpoint disables transport
// security, https:// keeps it. Everything else the exporters support
// (headers, timeout, compression, TLS material) is read from its own standard
// variable by the exporter itself.
//
// SECURITY: this service handles raw sensitive values, the client's
// Authorization header and the model's answer. None of that may reach a span.
// Spans carry request metadata only — see the attribute rules in
// internal/controller/gateway.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/logging"
)

// defaultServiceName is reported as service.name when OTEL_SERVICE_NAME is
// unset, so spans are attributable without extra configuration.
const defaultServiceName = "guardrails-llm-filter"

// Supported values of OTEL_EXPORTER_OTLP_PROTOCOL. The OTLP/JSON encoding of
// the specification has no Go exporter, so it is rejected at boot instead of
// silently falling back to another protocol.
const (
	protocolGRPC = "grpc"
	protocolHTTP = "http/protobuf"
)

const (
	envServiceName    = "OTEL_SERVICE_NAME"
	envEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
)

// Setup installs the W3C propagator and, when an OTLP endpoint is configured,
// the exporting tracer provider. It returns a shutdown function that flushes
// pending spans; with tracing disabled that is a no-op, so callers never
// special-case it. A configured but unusable pipeline (unknown protocol,
// malformed endpoint) is a boot error rather than a silently traceless process.
//
// The exporter connects lazily: an unreachable collector costs export retries
// in the background, never a failed or delayed boot.
func Setup(ctx context.Context, version string) (func(context.Context) error, error) {
	// Unconditional: continuing the caller's trace through this hop does not
	// depend on this service exporting spans of its own.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Also unconditional: where OpenTelemetry's own errors go is a process-wide
	// logging policy, not a property of exporting. The propagator and the SDK's
	// resource detection raise them with export off too, and the default handler
	// writes to stderr raw, bypassing GUARDRAILS_LOG_FORMAT / LOG_LEVEL.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logging.Warn(context.Background(), "OpenTelemetry error", "error", err)
	}))

	endpoint := envOrFallback(envTracesEndpoint, envEndpoint)
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	protocol := envOrFallback(envTracesProtocol, envProtocol)
	if protocol == "" {
		protocol = protocolGRPC
	}

	exporter, err := newExporter(ctx, protocol)
	if err != nil {
		return nil, err
	}

	res, err := newResource(version)
	if err != nil {
		return nil, err
	}

	// No WithSampler: the SDK then honours OTEL_TRACES_SAMPLER and
	// OTEL_TRACES_SAMPLER_ARG, defaulting to parentbased_always_on — a sampling
	// decision made by the caller (gateway) is respected for this hop.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(provider)

	logging.Info(ctx, "Tracing enabled", "otlp_endpoint", endpoint, "protocol", protocol)
	return provider.Shutdown, nil
}

// newExporter builds the span exporter for the configured protocol. Both
// exporters read their endpoint, headers, timeout, compression and TLS
// material from the standard OTEL_EXPORTER_OTLP_* variables themselves.
func newExporter(ctx context.Context, protocol string) (sdktrace.SpanExporter, error) {
	var (
		exporter sdktrace.SpanExporter
		err      error
	)
	switch protocol {
	case protocolGRPC:
		exporter, err = otlptracegrpc.New(ctx)
	case protocolHTTP:
		exporter, err = otlptracehttp.New(ctx)
	default:
		return nil, fmt.Errorf("%s must be one of %q, %q; got %q",
			envProtocol, protocolGRPC, protocolHTTP, protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("build OTLP trace exporter (%s): %w", protocol, err)
	}
	return exporter, nil
}

// newResource describes this process to the collector: the SDK defaults
// (telemetry.sdk.*, OTEL_RESOURCE_ATTRIBUTES) with service.name and
// service.version on top. The name is read here rather than left to
// resource.Default() so an unset OTEL_SERVICE_NAME yields this service's name
// instead of the SDK's "unknown_service:<binary>".
func newResource(version string) (*resource.Resource, error) {
	name := strings.TrimSpace(os.Getenv(envServiceName))
	if name == "" {
		name = defaultServiceName
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("build trace resource: %w", err)
	}
	return res, nil
}

// envOrFallback returns the first of the two variables that is set to a
// non-blank value, trimmed. Blank is treated as unset so an empty override in
// a deployment manifest disables the setting instead of configuring an empty
// endpoint or protocol.
func envOrFallback(primary, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(fallback))
}
