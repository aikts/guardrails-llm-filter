package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/models"
)

// TestUpstreamRequestNotReplayedAfterConnectionLoss pins that a request the
// upstream has already received is never sent to it a second time. The
// upstream reads the second request in full and drops the (reused, keep-alive)
// connection without answering — what a crashed or partitioned LLM backend
// looks like mid-generation. net/http replays such a request by itself when it
// can rewind the body and the request counts as idempotent, which an
// Idempotency-Key / X-Idempotency-Key header from the client makes it. For an
// LLM call a replay is a second model invocation (and a second charge), so
// the client must get the failure instead.
func TestUpstreamRequestNotReplayedAfterConnectionLoss(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		content string
		masker  *fakeMasker
	}{
		{name: "masked request, Idempotency-Key", header: "Idempotency-Key", content: "alice@example.com",
			masker: &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}},
		{name: "masked request, X-Idempotency-Key", header: "X-Idempotency-Key", content: "alice@example.com",
			masker: &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}},
		{name: "clean request, Idempotency-Key", header: "Idempotency-Key", content: "nothing to mask",
			masker: &fakeMasker{}},
		{name: "no idempotency header", content: "alice@example.com",
			masker: &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				if hits.Add(1) == 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[]}`))
			}))
			defer upstream.Close()

			h := newHandler(t, upstream.URL, tc.masker, enforceSettings(), nil)
			body := `{"messages":[{"role":"user","content":"` + tc.content + `"}]}`
			headers := map[string]string{}
			if tc.header != "" {
				headers[tc.header] = "key-1"
			}

			// The first request leaves a keep-alive connection in the pool; the
			// second one reuses it and loses it after the upstream has read it.
			first := doPost(t, h, "/v1/chat/completions", body, headers)
			_ = first.Body.Close()
			require.Equal(t, http.StatusOK, first.StatusCode)

			second := doPost(t, h, "/v1/chat/completions", body, headers)
			_ = second.Body.Close()

			assert.Equal(t, int32(2), hits.Load(), "the upstream must receive the lost request exactly once")
			assert.Equal(t, http.StatusBadGateway, second.StatusCode, "the connection loss is reported, not retried")
		})
	}
}
