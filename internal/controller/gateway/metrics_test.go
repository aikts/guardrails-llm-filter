package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/models"
)

// The duration histograms live in the global Prometheus registry, so these
// tests read before/after deltas and must not run in parallel: parallel tests
// of this package start only once the sequential ones are done.

var durationHistograms = []string{
	"extproc_guardrails_mask_duration_seconds",
	"extproc_guardrails_demask_duration_seconds",
	"extproc_guardrails_sse_chunk_demask_duration_seconds",
	"extproc_guardrails_pipeline_duration_seconds",
}

// histogramCounts returns the sample count of each duration histogram.
func histogramCounts(t *testing.T) map[string]uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	counts := make(map[string]uint64, len(durationHistograms))
	for _, name := range durationHistograms {
		counts[name] = 0
	}
	for _, f := range families {
		if _, ok := counts[f.GetName()]; ok && len(f.GetMetric()) > 0 {
			counts[f.GetName()] = f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	return counts
}

// observedDuring returns how many samples each duration histogram gained
// while fn ran.
func observedDuring(t *testing.T, fn func()) map[string]uint64 {
	t.Helper()
	before := histogramCounts(t)
	fn()
	after := histogramCounts(t)
	delta := make(map[string]uint64, len(after))
	for name, n := range after {
		delta[name] = n - before[name]
	}
	return delta
}

func TestMetrics_FullBodyObservesMaskDemaskAndPipeline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"reach <EMAIL_1> now"}}]}`))
	}))
	defer upstream.Close()
	masker := &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}
	h := newHandler(t, upstream.URL, masker, enforceSettings(), nil)

	delta := observedDuring(t, func() {
		resp := doPost(t, h, "/v1/chat/completions", `{"messages":[{"role":"user","content":"alice@example.com"}]}`, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	})

	assert.Equal(t, map[string]uint64{
		"extproc_guardrails_mask_duration_seconds":             1,
		"extproc_guardrails_demask_duration_seconds":           1,
		"extproc_guardrails_sse_chunk_demask_duration_seconds": 0,
		"extproc_guardrails_pipeline_duration_seconds":         1,
	}, delta)
}

func TestMetrics_StreamObservesMaskChunksAndPipeline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range []string{
			`data: {"choices":[{"delta":{"content":"reach <EMAIL_1>"}}]}`,
			`data: {"choices":[{"delta":{"content":" today"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()
	masker := &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}
	h := newHandler(t, upstream.URL, masker, enforceSettings(), nil)

	delta := observedDuring(t, func() {
		resp := doPost(t, h, "/v1/chat/completions",
			`{"stream":true,"messages":[{"role":"user","content":"alice@example.com"}]}`, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	})

	assert.Equal(t, uint64(1), delta["extproc_guardrails_mask_duration_seconds"])
	assert.Equal(t, uint64(0), delta["extproc_guardrails_demask_duration_seconds"], "a stream is not a full body")
	assert.GreaterOrEqual(t, delta["extproc_guardrails_sse_chunk_demask_duration_seconds"], uint64(1), "one sample per upstream read")
	assert.Equal(t, uint64(1), delta["extproc_guardrails_pipeline_duration_seconds"], "one sample per request, not per chunk")
}

// A request that is not masked at all (an unguarded path) costs the filter
// nothing, and is left out of the pipeline histogram rather than dragging its
// quantiles to zero.
func TestMetrics_UnguardedPathObservesNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	h := newHandler(t, upstream.URL, &fakeMasker{}, enforceSettings(), nil)

	delta := observedDuring(t, func() {
		resp := doPost(t, h, "/v1/embeddings", `{"input":"alice@example.com"}`, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	})

	for name, n := range delta {
		assert.Zero(t, n, name)
	}
}
