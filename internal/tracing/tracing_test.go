package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// clearOTLPEnv unsets every variable Setup reads, so a test starts from the
// "nothing configured" state regardless of the developer's shell.
func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envServiceName, envEndpoint, envTracesEndpoint, envProtocol, envTracesProtocol} {
		t.Setenv(name, "")
	}
}

// A blank value means "not configured", not "configured with an empty
// endpoint": deployment manifests neutralise a variable by setting it to "".
func TestSetup_DisabledWithoutEndpoint(t *testing.T) {
	for name, endpoint := range map[string]string{"unset": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			clearOTLPEnv(t)
			t.Setenv(envEndpoint, endpoint)

			before := otel.GetTracerProvider()
			shutdown, err := Setup(t.Context(), "test")
			require.NoError(t, err)
			require.NotNil(t, shutdown)
			assert.Same(t, before, otel.GetTracerProvider(), "no endpoint must not install a tracer provider")
			assert.NoError(t, shutdown(t.Context()))
		})
	}
}

// The propagator is installed even with export disabled: without it an inbound
// traceparent would be dropped and the caller's trace would break at this hop.
func TestSetup_InstallsPropagatorEvenWhenDisabled(t *testing.T) {
	clearOTLPEnv(t)

	_, err := Setup(t.Context(), "test")
	require.NoError(t, err)
	assert.Subset(t, otel.GetTextMapPropagator().Fields(), []string{"traceparent", "baggage"})
}

func TestSetup_EndpointEnablesExport(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "generic endpoint, default protocol",
			env:  map[string]string{envEndpoint: "http://127.0.0.1:4317"},
		},
		{
			name: "http/protobuf",
			env:  map[string]string{envEndpoint: "http://collector:4318/v1/traces", envProtocol: protocolHTTP},
		},
		{
			// A valid traces-specific protocol must win over a broken generic
			// one, which would otherwise fail the boot.
			name: "traces protocol wins over the generic one",
			env:  map[string]string{envEndpoint: "http://127.0.0.1:4318", envProtocol: "nonsense", envTracesProtocol: protocolHTTP},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOTLPEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			shutdown, err := Setup(t.Context(), "test")
			require.NoError(t, err)
			require.NotNil(t, shutdown)
			// The exporter connects lazily, so an unreachable collector must
			// not fail the boot — nor block the shutdown that flushes it.
			assert.NoError(t, shutdown(context.Background()))
		})
	}
}

// An unusable pipeline fails the boot instead of leaving a silently untraced
// process: OTLP/JSON has no Go exporter, and a typo must not fall back.
func TestSetup_UnknownProtocolIsABootError(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv(envEndpoint, "http://127.0.0.1:4317")
	t.Setenv(envProtocol, "http/json")

	_, err := Setup(t.Context(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), envProtocol)
	assert.Contains(t, err.Error(), "http/json")
}

// The traces-specific variable takes precedence over the generic one, and a
// blank value falls through to it rather than counting as configured.
func TestEnvOrFallback(t *testing.T) {
	tests := []struct {
		name            string
		traces, generic string
		want            string
	}{
		{name: "traces wins", traces: "http://traces:4317", generic: "http://generic:4317", want: "http://traces:4317"},
		{name: "blank traces falls back", traces: "  ", generic: "http://generic:4317", want: "http://generic:4317"},
		{name: "trimmed", traces: " http://traces:4317 ", want: "http://traces:4317"},
		{name: "both blank", traces: "", generic: " ", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envTracesEndpoint, tc.traces)
			t.Setenv(envEndpoint, tc.generic)
			assert.Equal(t, tc.want, envOrFallback(envTracesEndpoint, envEndpoint))
		})
	}
}

func TestNewResource_ServiceName(t *testing.T) {
	t.Run("falls back to the service's own name", func(t *testing.T) {
		clearOTLPEnv(t)

		res, err := newResource("1.2.3")
		require.NoError(t, err)
		assert.Equal(t, defaultServiceName, resourceAttr(t, res, semconv.ServiceNameKey))
		assert.Equal(t, "1.2.3", resourceAttr(t, res, semconv.ServiceVersionKey))
	})

	t.Run("an operator-set name wins", func(t *testing.T) {
		clearOTLPEnv(t)
		t.Setenv(envServiceName, "ap.guardrails-filter")

		res, err := newResource("1.2.3")
		require.NoError(t, err)
		assert.Equal(t, "ap.guardrails-filter", resourceAttr(t, res, semconv.ServiceNameKey))
	})
}

func resourceAttr(t *testing.T, res *resource.Resource, key attribute.Key) string {
	t.Helper()
	value, ok := res.Set().Value(key)
	require.Truef(t, ok, "resource has no %q", key)
	return value.AsString()
}
