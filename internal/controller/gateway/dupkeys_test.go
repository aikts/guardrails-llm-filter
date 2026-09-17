package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/models"
)

// captureUpstream records the body of the last request it received.
func captureUpstream(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream, &got
}

// userTexts decodes body the way a last-wins upstream (Python json, orjson,
// encoding/json) does and returns every text the model would read.
func userTexts(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	var texts []string
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			texts = append(texts, s)
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal(m.Content, &parts))
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			}
		}
	}
	return texts
}

// TestDuplicateKeysAreMaskedAsTheUpstreamReadsThem covers a body that repeats
// an object key: the extractors read the first value, while the upstream
// reads the last one, so without collapsing the model got unscanned text.
func TestDuplicateKeysAreMaskedAsTheUpstreamReadsThem(t *testing.T) {
	cases := map[string]string{
		"repeated content": `{"messages":[{"role":"user","content":"clean","content":"contact alice@example.com"}]}`,
		"repeated messages": `{"messages":[{"role":"user","content":"clean"}],` +
			`"messages":[{"role":"user","content":"contact alice@example.com"}]}`,
		"repeated part type": `{"messages":[{"role":"user","content":[` +
			`{"type":"image_url","type":"text","text":"contact alice@example.com"}]}]}`,
		"repeated in both values": `{"messages":[{"role":"user",` +
			`"content":"alice@example.com","content":"contact alice@example.com"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			upstream, got := captureUpstream(t)
			masker := &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}
			audit := &fakeAudit{}
			h := newHandler(t, upstream.URL, masker, enforceSettings(), audit)

			resp := doPost(t, h, "/v1/chat/completions", body, nil)
			defer func() { _ = resp.Body.Close() }()

			assert.NotContains(t, *got, "alice@example.com", "the original must not reach the upstream in any value")
			assert.Equal(t, []string{"contact <EMAIL_1>"}, userTexts(t, *got))
			require.Len(t, audit.calls, 1)
			assert.Equal(t, "pii.email", audit.calls[0].st.TriggeredRuleIDs[0])
		})
	}
}

func TestDuplicateKeysMaskedInEveryFormat(t *testing.T) {
	cases := map[string]string{
		"/v1/messages": `{"model":"m","max_tokens":1,"system":"clean","system":"alice@example.com",` +
			`"messages":[{"role":"user","content":"clean","content":"alice@example.com"}]}`,
		"/v1/responses": `{"model":"m","input":"clean","input":"alice@example.com"}`,
	}
	for path, body := range cases {
		t.Run(path, func(t *testing.T) {
			upstream, got := captureUpstream(t)
			masker := &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}
			h := newHandler(t, upstream.URL, masker, enforceSettings(), nil)

			resp := doPost(t, h, path, body, nil)
			defer func() { _ = resp.Body.Close() }()

			assert.NotContains(t, *got, "alice@example.com")
			assert.NotContains(t, *got, "clean", "only the last value of each key is forwarded")
			assert.Contains(t, *got, "<EMAIL_1>")
		})
	}
}

func TestDuplicateKeysCollapsedWithoutFindings(t *testing.T) {
	upstream, got := captureUpstream(t)
	h := newHandler(t, upstream.URL, &fakeMasker{}, enforceSettings(), nil)

	resp := doPost(t, h, "/v1/chat/completions",
		`{"stream":true,"messages":[{"role":"user","content":"a","content":"b"}],"stream":false}`, nil)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, `{"stream":false,"messages":[{"role":"user","content":"b"}]}`, *got,
		"enforce mode forwards exactly the body it scanned")
}

func TestDuplicateKeysStreamFlagReadsLastValue(t *testing.T) {
	upstream, _ := captureUpstream(t)
	masker := &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}
	audit := &fakeAudit{}
	h := newHandler(t, upstream.URL, masker, enforceSettings(), audit)

	resp := doPost(t, h, "/v1/chat/completions",
		`{"stream":false,"messages":[{"role":"user","content":"alice@example.com"}],"stream":true}`, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Len(t, audit.calls, 1)
	assert.True(t, audit.calls[0].md.IsStreaming, "the upstream streams, so the request is a streaming one")
}

func TestDuplicateKeysDetectModeForwardsOriginal(t *testing.T) {
	upstream, got := captureUpstream(t)
	global := enforceSettings()
	global.Mode = models.ModeDetect
	masker := &fakeMasker{reps: []models.Replacement{emailRep()}, ruleIDs: []string{"pii.email"}}
	audit := &fakeAudit{}
	h := newHandler(t, upstream.URL, masker, global, audit)

	body := `{"messages":[{"role":"user","content":"clean","content":"alice@example.com"}]}`
	resp := doPost(t, h, "/v1/chat/completions", body, nil)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, body, *got, "detect mode never changes the forwarded body")
	require.Len(t, audit.calls, 1, "detect mode records what the upstream reads")
	assert.Equal(t, []string{"<EMAIL_1>"}, audit.calls[0].maskedTexts)
}

func TestDuplicateKeysUnguardedPathUntouched(t *testing.T) {
	upstream, got := captureUpstream(t)
	h := newHandler(t, upstream.URL, &fakeMasker{}, enforceSettings(), nil)

	body := `{"input":"a","input":"b"}`
	resp := doPost(t, h, "/v1/embeddings", body, nil)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, body, *got)
}
