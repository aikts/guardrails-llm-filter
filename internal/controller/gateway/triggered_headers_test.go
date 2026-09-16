package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/config"
	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/controller/gateway"
	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/guardrails/demask"
	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/models"
)

// The masking-outcome response headers under their default names.
const (
	hdrDataTypes = "X-Guardrails-Data-Types-Triggered"
	hdrRules     = "X-Guardrails-Triggered-Rules"
	hdrCounts    = "X-Guardrails-Replacement-Counts"
)

var outcomeHeaders = []string{hdrDataTypes, hdrRules, hdrCounts}

// headersConfig is testConfig with the outcome header names at their defaults
// and exposure set as given.
func headersConfig(upstreamURL string, expose bool) *config.Config {
	cfg := testConfig(upstreamURL)
	cfg.GuardrailsHeaders = config.GuardrailsHeaders{
		DataTypesHeader:         "x-guardrails-data-types-triggered",
		TriggeredRulesHeader:    "x-guardrails-triggered-rules",
		ReplacementCountsHeader: "x-guardrails-replacement-counts",
		ExposeTriggeredRules:    expose,
	}
	return cfg
}

func assertNoOutcomeHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for _, name := range outcomeHeaders {
		assert.Empty(t, h.Values(name), "%s must be absent", name)
	}
}

// piiText carries two names (one of them twice) and a personal INN.
const piiText = "Иванов Иван Иванович и Петров Пётр Петрович, ИНН 500100732259. Повторно: Иванов Иван Иванович"

// piiOutcome is what the shipped rules report for piiText: the repeated name
// reuses its placeholder, so pii.fio-ru replaced two distinct values.
var piiOutcome = map[string]string{
	hdrDataTypes: "5",
	hdrRules:     "pii.docs.inn-person,pii.fio-ru",
	hdrCounts:    "pii.docs.inn-person=1,pii.fio-ru=2",
}

func assertPIIOutcome(t *testing.T, h http.Header) {
	t.Helper()
	for name, want := range piiOutcome {
		assert.Equal(t, []string{want}, h.Values(name), name)
	}
}

func TestTriggeredHeaders_FullBody(t *testing.T) {
	t.Parallel()
	seen := &capture{}
	up := echoUpstream(t, seen)
	defer up.Close()
	h := realGatewayHandlerWith(t, headersConfig(up.URL, true), enforceSettings(), &fakeAudit{})

	resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiText, false), nil)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assertPIIOutcome(t, resp.Header)

	assert.Equal(t, "<FIO_1> и <FIO_2>, ИНН <INN_PERSON_1>. Повторно: <FIO_1>", seen.get(),
		"the upstream must receive placeholders only")
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(body, &out), "response must be valid JSON: %s", body)
	require.Len(t, out.Choices, 1)
	assert.Equal(t, "You said: "+piiText, out.Choices[0].Message.Content, "the client must get the originals back")
}

// TestTriggeredHeaders_CountsMatchMaskedBody cross-checks the counts header
// against the request the upstream actually received: for every rule, the
// count equals the number of distinct placeholders of that rule in the
// forwarded body, and the two rule headers name the same rules.
func TestTriggeredHeaders_CountsMatchMaskedBody(t *testing.T) {
	t.Parallel()
	placeholderRe := regexp.MustCompile(`<[A-Za-z0-9_]+_[0-9]+>`)

	texts := []string{piiText}
	for _, tc := range roundTripCases {
		texts = append(texts, tc.text)
	}
	for i, text := range texts {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			seen := &capture{}
			up := echoUpstream(t, seen)
			defer up.Close()
			audit := &fakeAudit{}
			h := realGatewayHandlerWith(t, headersConfig(up.URL, true), enforceSettings(), audit)

			resp := doPost(t, h, "/v1/chat/completions", chatBody(t, text, false), nil)
			_ = resp.Body.Close()

			if len(audit.calls) == 0 {
				assertNoOutcomeHeaders(t, resp.Header)
				return
			}
			ruleOf := make(map[string]string) // placeholder -> rule
			for _, rep := range audit.calls[0].st.Replacements {
				ruleOf[rep.Placeholder] = rep.RuleID
			}
			// Distinct placeholders the service introduced into the forwarded body.
			// A placeholder-like literal already in the input is not one of them.
			inBody := make(map[string]int) // rule -> distinct placeholders
			for _, ph := range uniq(placeholderRe.FindAllString(seen.get(), -1)) {
				if rule, ok := ruleOf[ph]; ok {
					inBody[rule]++
				}
			}

			rules := strings.Split(resp.Header.Get(hdrRules), ",")
			counts := parseCounts(t, resp.Header.Get(hdrCounts))
			require.Len(t, counts, len(rules))
			for i, rule := range rules {
				assert.Equal(t, rule, counts[i].rule, "both headers list the same rules in the same order")
				assert.Equal(t, inBody[rule], counts[i].n, "replacements of %s", rule)
			}
		})
	}
}

// TestTriggeredHeaders_SSE checks that the headers arrive with the stream's
// first bytes — before the upstream has finished — and that a placeholder
// split across SSE frames is still restored.
func TestTriggeredHeaders_SSE(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var heldTooLong atomic.Bool
	var llmSaw capture
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req upstreamReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		content := lastUserContent(req)
		llmSaw.set(content)

		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		frame := func(delta map[string]any) {
			chunk, _ := json.Marshal(map[string]any{
				"object":  "chat.completion.chunk",
				"choices": []map[string]any{{"index": 0, "delta": delta}},
			})
			_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
			fl.Flush()
		}
		frame(map[string]any{"role": "assistant"})
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			heldTooLong.Store(true)
		}
		// Three runes per frame: every placeholder is torn across frames.
		reply := []rune("You said: " + content)
		for i := 0; i < len(reply); i += 3 {
			frame(map[string]any{"content": string(reply[i:min(i+3, len(reply))])})
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	gw := httptest.NewServer(realGatewayHandlerWith(t, headersConfig(up.URL, true), enforceSettings(), &fakeAudit{}))
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(t, piiText, true)))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	close(release)

	assert.False(t, heldTooLong.Load(), "the headers must reach the client while the upstream is still streaming")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	assertPIIOutcome(t, resp.Header)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assembled, done := parseSSE(t, body)
	assert.True(t, done)
	assert.Equal(t, "You said: "+piiText, assembled, "split placeholders must be restored")
	assert.NotContains(t, llmSaw.get(), "500100732259")
}

// TestTriggeredHeaders_ExposeDisabled checks that, as in the ext_proc variant,
// exposure gates only the rule headers: the data-types header is still set.
func TestTriggeredHeaders_ExposeDisabled(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			t.Parallel()
			seen := &capture{}
			up := echoUpstream(t, seen)
			defer up.Close()
			h := realGatewayHandlerWith(t, headersConfig(up.URL, false), enforceSettings(), &fakeAudit{})

			resp := doPost(t, h, "/v1/chat/completions", chatBody(t, piiText, stream), nil)
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)

			assert.Equal(t, []string{piiOutcome[hdrDataTypes]}, resp.Header.Values(hdrDataTypes))
			assert.Empty(t, resp.Header.Values(hdrRules))
			assert.Empty(t, resp.Header.Values(hdrCounts))
			assert.Contains(t, seen.get(), "<INN_PERSON_1>", "masking itself does not depend on exposure")
			assert.NotContains(t, string(body), "<INN_PERSON_1>")
		})
	}
}

func TestTriggeredHeaders_AbsentWithoutMasking(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		text   string
		global func() models.GuardrailsSettings
	}{
		{"no PII", "просто обычный текст без персональных данных", enforceSettings},
		{"detect mode", piiText, func() models.GuardrailsSettings {
			s := enforceSettings()
			s.Mode = models.ModeDetect
			return s
		}},
	}
	for _, tc := range cases {
		for _, expose := range []bool{true, false} {
			for _, stream := range []bool{false, true} {
				name := tc.name + "/expose=" + strconv.FormatBool(expose) + "/stream=" + strconv.FormatBool(stream)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					up := echoUpstream(t, &capture{})
					defer up.Close()
					h := realGatewayHandlerWith(t, headersConfig(up.URL, expose), tc.global(), &fakeAudit{})

					resp := doPost(t, h, "/v1/chat/completions", chatBody(t, tc.text, stream), nil)
					defer func() { _ = resp.Body.Close() }()
					_, _ = io.ReadAll(resp.Body)

					require.Equal(t, http.StatusOK, resp.StatusCode)
					assertNoOutcomeHeaders(t, resp.Header)
				})
			}
		}
	}
}

// TestTriggeredHeaders_UpstreamValues checks that the service owns the names
// of the headers it reports on — the data-types header always, the rule
// headers while exposure is on: whatever the upstream sent under them never
// reaches the client. With exposure off the rule headers are relayed like any
// header.
func TestTriggeredHeaders_UpstreamValues(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, name := range outcomeHeaders {
			w.Header().Set(name, "spoofed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)

	const noPII = "без персональных данных"
	// relayedRuleHeaders checks the data-types header against want (nil: absent)
	// and that the upstream's rule headers came through untouched.
	relayedRuleHeaders := func(want []string) func(t *testing.T, h http.Header) {
		return func(t *testing.T, h http.Header) {
			t.Helper()
			assert.Equal(t, want, h.Values(hdrDataTypes), hdrDataTypes)
			for _, name := range []string{hdrRules, hdrCounts} {
				assert.Equal(t, []string{"spoofed"}, h.Values(name), name)
			}
		}
	}

	cases := []struct {
		name   string
		expose bool
		text   string
		check  func(t *testing.T, h http.Header)
	}{
		{"exposed, not masked", true, noPII, assertNoOutcomeHeaders},
		{"exposed, masked", true, piiText, assertPIIOutcome},
		{"not exposed, not masked", false, noPII, relayedRuleHeaders(nil)},
		{"not exposed, masked", false, piiText, relayedRuleHeaders([]string{piiOutcome[hdrDataTypes]})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := realGatewayHandlerWith(t, headersConfig(up.URL, tc.expose), enforceSettings(), &fakeAudit{})
			resp := doPost(t, h, "/v1/chat/completions", chatBody(t, tc.text, false), nil)
			defer func() { _ = resp.Body.Close() }()
			tc.check(t, resp.Header)
		})
	}
}

// TestTriggeredHeaders_Format pins the value format on a state with several
// data types and a rule that triggered without a replacement of its own, and
// shows that an empty header name disables that header.
func TestTriggeredHeaders_Format(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(upstream.Close)

	masker := func() *fakeMasker {
		return &fakeMasker{
			reps: []models.Replacement{
				{RuleID: "credentials.password", Original: "hunter2", Placeholder: "<PASSWORD_1>"},
				{RuleID: "pii.email", Original: "alice@example.com", Placeholder: "<EMAIL_1>"},
				{RuleID: "pii.email", Original: "bob@example.com", Placeholder: "<EMAIL_2>"},
			},
			ruleIDs:   []string{"credentials.password", "pii.email", "pii.email.context"},
			dataTypes: []models.DataType{models.DataTypeCREDENTIALS, models.DataTypePERSONALDATA},
		}
	}
	const text = `{"messages":[{"role":"user","content":"hunter2 alice@example.com bob@example.com"}]}`

	t.Run("all headers", func(t *testing.T) {
		t.Parallel()
		h := newHandlerWithConfig(t, headersConfig(upstream.URL, true), masker())
		resp := doPost(t, h, "/v1/chat/completions", text, nil)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, []string{"1,5"}, resp.Header.Values(hdrDataTypes))
		assert.Equal(t, []string{"credentials.password,pii.email,pii.email.context"}, resp.Header.Values(hdrRules))
		assert.Equal(t, []string{"credentials.password=1,pii.email=2,pii.email.context=0"}, resp.Header.Values(hdrCounts))
	})

	t.Run("empty name disables a header", func(t *testing.T) {
		t.Parallel()
		cfg := headersConfig(upstream.URL, true)
		cfg.GuardrailsHeaders.DataTypesHeader = ""
		h := newHandlerWithConfig(t, cfg, masker())
		resp := doPost(t, h, "/v1/chat/completions", text, nil)
		defer func() { _ = resp.Body.Close() }()

		assert.Empty(t, resp.Header.Values(hdrDataTypes))
		assert.NotEmpty(t, resp.Header.Values(hdrRules))
		assert.NotEmpty(t, resp.Header.Values(hdrCounts))
	})
}

func newHandlerWithConfig(t *testing.T, cfg *config.Config, masker gateway.Masker) *gateway.Handler {
	t.Helper()
	provider := demask.NewProvider(fakeDemaskReg{}, fakeScanner{})
	h, err := gateway.New(cfg, masker, &fakeSettings{global: enforceSettings()}, provider, nil)
	require.NoError(t, err)
	return h
}

type ruleCount struct {
	rule string
	n    int
}

func parseCounts(t *testing.T, header string) []ruleCount {
	t.Helper()
	var out []ruleCount
	for entry := range strings.SplitSeq(header, ",") {
		rule, raw, ok := strings.Cut(entry, "=")
		require.True(t, ok, "entry %q is not rule_id=count", entry)
		n, err := strconv.Atoi(raw)
		require.NoError(t, err, "entry %q", entry)
		out = append(out, ruleCount{rule, n})
	}
	return out
}

func uniq(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := items[:0:0]
	for _, it := range items {
		if _, ok := seen[it]; !ok {
			seen[it] = struct{}{}
			out = append(out, it)
		}
	}
	return out
}
