package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/tracing"
)

// Tracing of the data plane. The recorder is installed once for the whole
// package: the otel global provider delegates to the FIRST provider set, so a
// per-test provider would be ignored. Tests therefore isolate themselves by
// trace ID — each sends its own traceparent and reads back only that trace.

var spanRecorder *tracetest.SpanRecorder

func TestMain(m *testing.M) {
	// Same wiring as production: Setup installs the W3C propagator even with
	// tracing disabled (no OTLP endpoint is configured here), and the recorder
	// stands in for the exporting provider.
	if _, err := tracing.Setup(context.Background(), "test"); err != nil {
		panic(err)
	}
	spanRecorder = tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder)))
	os.Exit(m.Run())
}

// traceparent builds a sampled W3C traceparent for a caller-chosen trace, so a
// test can find its own spans in the shared recorder.
func traceparent(t *testing.T, traceIDHex string) (trace.TraceID, string) {
	t.Helper()
	traceID, err := trace.TraceIDFromHex(traceIDHex)
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("b7ad6b7169203331")
	require.NoError(t, err)
	return traceID, "00-" + traceID.String() + "-" + spanID.String() + "-01"
}

// spansOf returns the recorded spans belonging to one trace.
func spansOf(t *testing.T, traceID trace.TraceID) []sdktrace.ReadOnlySpan {
	t.Helper()
	var out []sdktrace.ReadOnlySpan
	for _, span := range spanRecorder.Ended() {
		if span.SpanContext().TraceID() == traceID {
			out = append(out, span)
		}
	}
	return out
}

func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	require.Failf(t, "span not found", "no span named %q among %v", name, spanNames(spans))
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, span := range spans {
		names[i] = span.Name()
	}
	return names
}

func attrOf(t *testing.T, span sdktrace.ReadOnlySpan, key string) attribute.Value {
	t.Helper()
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	require.Failf(t, "attribute not found", "span %q has no attribute %q", span.Name(), key)
	return attribute.Value{}
}

const piiPrompt = "Иванов Иван Петрович, тел +7 900 123-45-67, ИНН 500100732259"

func TestTracing_SpanPerPipelinePhase(t *testing.T) {
	seen := &capture{}
	up := echoUpstream(t, seen)
	defer up.Close()
	h := realGatewayHandler(t, up.URL)

	traceID, header := traceparent(t, "0af7651916cd43dd8448eb211c80319c")
	resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiPrompt, false),
		map[string]string{"Content-Type": "application/json", "traceparent": header})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	spans := spansOf(t, traceID)
	server := spanNamed(t, spans, "POST /v1/chat/completions")
	mask := spanNamed(t, spans, "guardrails.mask")
	upstream := spanNamed(t, spans, "guardrails.upstream")
	demask := spanNamed(t, spans, "guardrails.demask")

	// The caller's span parents ours, and every phase hangs under the server
	// span: one unbroken trace from the caller through this hop.
	assert.Equal(t, "b7ad6b7169203331", server.Parent().SpanID().String())
	assert.Equal(t, trace.SpanKindServer, server.SpanKind())
	for _, child := range []sdktrace.ReadOnlySpan{mask, upstream, demask} {
		assert.Equal(t, server.SpanContext().SpanID(), child.Parent().SpanID(), "parent of %q", child.Name())
	}
	assert.Equal(t, trace.SpanKindClient, upstream.SpanKind())

	assert.Equal(t, "/v1/chat/completions", attrOf(t, server, "guardrails.route").AsString())
	assert.Equal(t, "chat_completions", attrOf(t, server, "guardrails.dialect").AsString())
	assert.True(t, attrOf(t, server, "guardrails.guarded").AsBool())
	assert.True(t, attrOf(t, server, "guardrails.demasked").AsBool())
	assert.Positive(t, attrOf(t, server, "guardrails.request_body_bytes").AsInt64())

	assert.Equal(t, "masked", attrOf(t, mask, "guardrails.outcome").AsString())
	assert.Positive(t, attrOf(t, mask, "guardrails.rules_triggered").AsInt64())
	assert.NotEmpty(t, attrOf(t, mask, "guardrails.rule_ids").AsStringSlice())
	assert.Positive(t, attrOf(t, mask, "guardrails.replacements").AsInt64())

	assert.Equal(t, int64(http.StatusOK), attrOf(t, upstream, "http.response.status_code").AsInt64())
	// The client's outcome belongs on the server span, next to the request it
	// answers: the status it receives and the size of the body it gets back.
	assert.Equal(t, int64(http.StatusOK), attrOf(t, server, "http.response.status_code").AsInt64())
	assert.Positive(t, attrOf(t, server, "guardrails.response_body_bytes").AsInt64())
	assert.Empty(t, demask.Attributes(), "the demask span is a timing span only")
}

func TestTracing_UpstreamRequestCarriesOurSpan(t *testing.T) {
	var gotTraceparent string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer up.Close()
	h := realGatewayHandler(t, up.URL)

	traceID, header := traceparent(t, "1af7651916cd43dd8448eb211c80319c")
	resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiPrompt, false),
		map[string]string{"Content-Type": "application/json", "traceparent": header})
	defer func() { _ = resp.Body.Close() }()

	upstream := spanNamed(t, spansOf(t, traceID), "guardrails.upstream")
	// Same trace, and the provider's own spans hang under OUR upstream span
	// rather than beside it: the client's traceparent was replaced, not copied.
	require.NotEmpty(t, gotTraceparent)
	assert.Equal(t, "00-"+traceID.String()+"-"+upstream.SpanContext().SpanID().String()+"-01", gotTraceparent)
}

func TestTracing_StreamingResponseGetsItsOwnSpan(t *testing.T) {
	seen := &capture{}
	up := echoUpstream(t, seen)
	defer up.Close()
	h := realGatewayHandler(t, up.URL)

	traceID, header := traceparent(t, "2af7651916cd43dd8448eb211c80319c")
	resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiPrompt, true),
		map[string]string{"Content-Type": "application/json", "traceparent": header})
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	spans := spansOf(t, traceID)
	server := spanNamed(t, spans, "POST /v1/chat/completions")
	// The stream gets its own span, and the server span says it was one.
	sse := spanNamed(t, spans, "guardrails.demask.sse")
	assert.Equal(t, server.SpanContext().SpanID(), sse.Parent().SpanID())
	assert.True(t, attrOf(t, server, "guardrails.streaming").AsBool())
}

func TestTracing_UnguardedPathNeverNamesTheClientPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	h := realGatewayHandler(t, up.URL)

	traceID, header := traceparent(t, "3af7651916cd43dd8448eb211c80319c")
	// The path of an unguarded request is client-controlled and unbounded, so
	// it must reach neither the span name nor an attribute.
	const secretPath = "/files/s3cr3t-customer-export"
	resp := doPost(t, h, secretPath, `{}`,
		map[string]string{"Content-Type": "application/json", "traceparent": header})
	defer func() { _ = resp.Body.Close() }()

	spans := spansOf(t, traceID)
	server := spanNamed(t, spans, "POST (unguarded)")
	assert.Equal(t, "(unguarded)", attrOf(t, server, "guardrails.route").AsString())
	assert.False(t, attrOf(t, server, "guardrails.guarded").AsBool())
	for _, text := range spanTexts(spans) {
		assert.NotContains(t, text, "s3cr3t", "the client path must not reach a span")
	}
}

func TestTracing_UpstreamFailureMarksTheSpans(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	up.Close() // nothing is listening: the upstream call fails to connect

	h := realGatewayHandler(t, up.URL)
	traceID, header := traceparent(t, "4af7651916cd43dd8448eb211c80319c")
	resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiPrompt, false),
		map[string]string{"Content-Type": "application/json", "traceparent": header})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	spans := spansOf(t, traceID)
	upstream := spanNamed(t, spans, "guardrails.upstream")
	server := spanNamed(t, spans, "POST /v1/chat/completions")
	assert.Equal(t, codes.Error, upstream.Status().Code)
	assert.Equal(t, codes.Error, server.Status().Code)
	// A fixed description, never the error value: it wraps the outbound URL,
	// whose query string is client-controlled.
	assert.Equal(t, "upstream request failed", upstream.Status().Description)
	assert.Equal(t, "upstream request failed", server.Status().Description)
	assert.NotContains(t, strings.Join(spanTexts(spans), " "), up.URL)
}

// placeholderRE matches the masking placeholders (<EMAIL_1>, <FIO_2>, …) that
// stand in for the values the model must not see.
var placeholderRE = regexp.MustCompile(`<[A-Z][A-Z0-9_]*_\d+>`)

// spanTexts flattens everything a span carries that a trace store would show:
// its name, its status description, its attribute keys and values, and its
// events.
func spanTexts(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, span := range spans {
		out = append(out, span.Name(), span.Status().Description)
		for _, kv := range span.Attributes() {
			out = append(out, string(kv.Key), kv.Value.String())
		}
		for _, event := range span.Events() {
			out = append(out, event.Name)
			for _, kv := range event.Attributes {
				out = append(out, string(kv.Key), kv.Value.String())
			}
		}
	}
	return out
}

// TestTracing_SpansCarryNoSensitiveData is the invariant that makes tracing
// safe to enable on a service that handles raw personal data: a span describes
// the request, never its content. It guards the whole span surface — names,
// statuses, attribute keys and values, events — of a request that carries
// personal data, a bearer token and a model name.
func TestTracing_SpansCarryNoSensitiveData(t *testing.T) {
	seen := &capture{}
	up := echoUpstream(t, seen)
	defer up.Close()
	h := realGatewayHandler(t, up.URL)

	const bearer = "sk-super-secret-client-key"
	traceID, header := traceparent(t, "5af7651916cd43dd8448eb211c80319c")
	resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiPrompt, false), map[string]string{
		"Content-Type":  "application/json",
		"traceparent":   header,
		"Authorization": "Bearer " + bearer,
		"X-Request-Id":  "req-42",
	})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The masker really fired, so the placeholders below are not vacuous.
	require.Regexp(t, placeholderRE, seen.get(), "upstream must have received placeholders")

	spans := spansOf(t, traceID)
	require.NotEmpty(t, spans)
	texts := spanTexts(spans)

	forbidden := []string{
		"Иванов Иван Петрович", // a value the masker replaced
		"+7 900 123-45-67",
		"500100732259",
		bearer,      // the client's credential
		"You said:", // the model's answer
		"gpt",       // the model name: metadata we deliberately do not record
		"req-42",    // the client's request id, which keys the audit record
	}
	for _, text := range texts {
		for _, secret := range forbidden {
			assert.NotContains(t, text, secret, "span surface must not carry %q", secret)
		}
		assert.NotRegexp(t, placeholderRE, text, "span surface must not carry a placeholder")
		// No header ever becomes an attribute, by name or by value.
		assert.NotContains(t, strings.ToLower(text), "authorization")
		assert.NotContains(t, strings.ToLower(text), "http.request.header")
	}
}
