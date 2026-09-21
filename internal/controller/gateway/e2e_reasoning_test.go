package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// reasoningShape is how an upstream names the reasoning it streams.
type reasoningShape string

const (
	// reasoningContentOnly: DeepSeek, vLLM, LiteLLM relaying most providers.
	reasoningContentOnly reasoningShape = "reasoning_content"
	// reasoningOpenRouter: LiteLLM relaying OpenRouter — the same text as
	// reasoning, reasoning_content and a reasoning_details entry.
	reasoningOpenRouter reasoningShape = "openrouter"
)

// reasoningEchoUpstream is echoUpstream for a reasoning model: it echoes
// "You said: <content>" as the model's reasoning — streamed one rune per
// frame, so every placeholder is cut across frames — then answers "ok".
func reasoningEchoUpstream(t *testing.T, seen *capture, shape reasoningShape) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req upstreamReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		content := lastUserContent(req)
		seen.set(content)
		reply := "You said: " + content

		reasoningFields := func(text string) map[string]any {
			fields := map[string]any{"reasoning_content": text}
			if shape == reasoningOpenRouter {
				fields["reasoning"] = text
				fields["reasoning_details"] = []map[string]any{{
					"type": "reasoning.text", "text": text, "format": "unknown", "index": 0,
				}}
			}
			return fields
		}

		if !req.Stream {
			message := reasoningFields(reply)
			message["role"] = "assistant"
			message["content"] = "ok"
			if shape == reasoningOpenRouter {
				// LiteLLM moves OpenRouter's extras of a full response here.
				message["provider_specific_fields"] = map[string]any{
					"reasoning":         reply,
					"reasoning_details": message["reasoning_details"],
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion",
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": message}},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(choice map[string]any) {
			chunk, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk",
				"choices": []map[string]any{choice},
			})
			_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		send(map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}})
		for _, r := range reply {
			send(map[string]any{"index": 0, "delta": reasoningFields(string(r))})
		}
		send(map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop"})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
}

// TestE2E_SSE_ReasoningRoundTrip is the round trip for a reasoning model's
// stream: with the real masker and demasker, the reasoning the client
// assembles — under every name the upstream used — is the original text,
// although each placeholder reached the stream cut into single runes.
func TestE2E_SSE_ReasoningRoundTrip(t *testing.T) {
	t.Parallel()
	jsonHdr := map[string]string{"Content-Type": "application/json"}

	for _, shape := range []reasoningShape{reasoningContentOnly, reasoningOpenRouter} {
		seen := &capture{}
		up := reasoningEchoUpstream(t, seen, shape)
		defer up.Close()
		h := realGatewayHandler(t, up.URL)

		for _, tc := range roundTripCases {
			t.Run(string(shape)+"/"+tc.name, func(t *testing.T) {
				resp := doPost(t, h, "/v1/chat/completions", chatBody(t, tc.text, true), jsonHdr)
				defer func() { _ = resp.Body.Close() }()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				body, _ := io.ReadAll(resp.Body)

				var reasoning, reasoningContent, details, content strings.Builder
				for _, frame := range strings.Split(string(body), "\n\n") {
					payload := strings.TrimPrefix(strings.TrimSpace(frame), "data: ")
					if payload == "" || payload == "[DONE]" {
						continue
					}
					require.True(t, gjson.Valid(payload), "SSE data must be valid JSON: %q", payload)
					delta := gjson.Get(payload, "choices.0.delta")
					reasoning.WriteString(delta.Get("reasoning").String())
					reasoningContent.WriteString(delta.Get("reasoning_content").String())
					for _, d := range delta.Get("reasoning_details").Array() {
						details.WriteString(d.Get("text").String())
					}
					content.WriteString(delta.Get("content").String())
				}

				if tc.name == "pii-rich" {
					require.NotContains(t, seen.get(), "Иванов", "the upstream must see placeholders, or nothing is demasked")
				}
				want := "You said: " + tc.text
				assert.Equal(t, want, reasoningContent.String())
				if shape == reasoningOpenRouter {
					assert.Equal(t, want, reasoning.String())
					assert.Equal(t, want, details.String())
				} else {
					assert.Empty(t, reasoning.String(), "no reasoning field the upstream never sent")
					assert.Empty(t, details.String())
				}
				assert.Equal(t, "ok", content.String())
			})
		}
	}
}

func TestE2E_NonStreaming_ReasoningRoundTrip(t *testing.T) {
	t.Parallel()
	seen := &capture{}
	up := reasoningEchoUpstream(t, seen, reasoningOpenRouter)
	defer up.Close()
	h := realGatewayHandler(t, up.URL)
	jsonHdr := map[string]string{"Content-Type": "application/json"}

	for _, tc := range roundTripCases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doPost(t, h, "/v1/chat/completions", chatBody(t, tc.text, false), jsonHdr)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, _ := io.ReadAll(resp.Body)
			require.True(t, gjson.ValidBytes(body), "response must be valid JSON: %s", body)

			if tc.name == "pii-rich" {
				require.NotContains(t, seen.get(), "Иванов", "the upstream must see placeholders, or nothing is demasked")
			}
			message := gjson.GetBytes(body, "choices.0.message")
			want := "You said: " + tc.text
			assert.Equal(t, want, message.Get("reasoning").String())
			assert.Equal(t, want, message.Get("reasoning_content").String())
			assert.Equal(t, want, message.Get("reasoning_details.0.text").String())
			assert.Equal(t, want, message.Get("provider_specific_fields.reasoning").String())
			assert.Equal(t, want, message.Get("provider_specific_fields.reasoning_details.0.text").String())
		})
	}
}
