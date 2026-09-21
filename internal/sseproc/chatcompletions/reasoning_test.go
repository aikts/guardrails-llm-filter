package chatcompletions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/sseproc/common"
)

// placeholdersDemasker is placeholderDemasker for several placeholders: each
// complete occurrence is replaced, and a trailing prefix of any placeholder is
// withheld until a later chunk completes it or a flush releases it.
func placeholdersDemasker(originals map[string]string) common.DemaskerFactoryFn {
	return func() common.Demasker {
		pending := ""
		return newDemaskerMock(func(_ context.Context, chunk string, flush bool) (string, error) {
			text := pending + chunk
			pending = ""
			for ph, orig := range originals {
				text = strings.ReplaceAll(text, ph, orig)
			}
			if flush {
				return text, nil
			}
			for i := strings.LastIndexByte(text, '<'); i >= 0; i = -1 {
				for ph := range originals {
					if strings.HasPrefix(ph, text[i:]) {
						pending = text[i:]
						return text[:i], nil
					}
				}
			}
			return text, nil
		})
	}
}

var testOriginals = map[string]string{
	"<FIO_1>":         "Иванов Иван Петрович",
	"<PASSPORT_RF_1>": "4510 123456",
}

// reasoningStream is what a client assembles from the demasked stream.
type reasoningStream struct {
	frames           []map[string]any // every choice-0 delta, in order
	reasoning        strings.Builder
	reasoningContent strings.Builder
	details          strings.Builder // concatenated reasoning_details[].text
	content          strings.Builder
}

func collectReasoningStream(t *testing.T, out []byte) *reasoningStream {
	t.Helper()
	s := &reasoningStream{}
	frames, _ := splitFrames(out)
	for _, f := range frames {
		payload := strings.TrimPrefix(strings.TrimSpace(string(f)), "data: ")
		if payload == "[DONE]" {
			continue
		}
		require.True(t, gjson.Valid(payload), "frame must carry valid JSON: %q", payload)
		delta := gjson.Get(payload, "choices.0.delta")
		if !delta.Exists() {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(delta.Raw), &m))
		s.frames = append(s.frames, m)
		s.reasoning.WriteString(delta.Get("reasoning").String())
		s.reasoningContent.WriteString(delta.Get("reasoning_content").String())
		for _, d := range delta.Get("reasoning_details").Array() {
			s.details.WriteString(d.Get("text").String())
		}
		s.content.WriteString(delta.Get("content").String())
	}
	return s
}

func feedFrames(t *testing.T, p *Processor, frames []string, eos bool) []byte {
	t.Helper()
	var out []byte
	for i, f := range frames {
		got, err := p.ProcessChunk(context.Background(), []byte(f), eos && i == len(frames)-1)
		require.NoError(t, err)
		out = append(out, got...)
	}
	return out
}

func dataFrame(delta string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":` + delta + `}]}` + "\n\n"
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// assertNoPlaceholderLeak fails when any placeholder, whole or cut, reached
// the client.
func assertNoPlaceholderLeak(t *testing.T, out []byte) {
	t.Helper()
	for _, frag := range []string{"<FIO", "<PASSPORT", "O_1>", "RF_1>", "_1>"} {
		assert.NotContains(t, string(out), frag, "placeholder fragment leaked to the client")
	}
}

// The regression the live check found: a reasoning model streams its
// chain-of-thought in delta.reasoning_content, which the processor did not
// model, so the frame was relayed verbatim with the placeholders in it.
func TestReasoningContent_SplitPlaceholderDemasked(t *testing.T) {
	parts := []string{"Клиент <FI", "O_1>, паспорт <PASSPORT_", "RF_1>", ". Готово"}
	var frames []string
	for _, part := range parts {
		frames = append(frames, dataFrame(`{"reasoning_content":`+jsonString(part)+`}`))
	}
	frames = append(frames,
		dataFrame(`{"content":"Ответ"}`),
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)

	out := feedFrames(t, New(placeholdersDemasker(testOriginals)), frames, true)

	s := collectReasoningStream(t, out)
	assert.Equal(t, "Клиент Иванов Иван Петрович, паспорт 4510 123456. Готово", s.reasoningContent.String())
	assert.Equal(t, "Ответ", s.content.String())
	assert.Empty(t, s.reasoning.String(), "the stream's field name is kept: no reasoning field invented")
	assertNoPlaceholderLeak(t, out)
}

// LiteLLM relaying OpenRouter sends the same text as reasoning,
// reasoning_content and a reasoning_details entry in one delta. They must go
// out together, one frame per upstream delta, or a client reading
// `reasoning_content or reasoning` shows the text twice.
func TestReasoningFields_OneFramePerDelta(t *testing.T) {
	parts := []string{"Клиент <FI", "O_1> и", " паспорт <PASSPORT_RF_1>"}
	var frames []string
	for _, part := range parts {
		js := jsonString(part)
		frames = append(frames, dataFrame(`{"reasoning":`+js+`,"reasoning_content":`+js+
			`,"reasoning_details":[{"type":"reasoning.text","text":`+js+`,"format":"anthropic-claude-v1","index":0}]}`))
	}
	frames = append(frames, dataFrame(`{"reasoning_details":[{"type":"reasoning.text","text":"","signature":"c2ln","format":"anthropic-claude-v1","index":0}]}`),
		dataFrame(`{"content":"Ок"}`), "data: [DONE]\n\n")

	out := feedFrames(t, New(placeholdersDemasker(testOriginals)), frames, true)
	s := collectReasoningStream(t, out)

	want := "Клиент Иванов Иван Петрович и паспорт 4510 123456"
	assert.Equal(t, want, s.reasoning.String())
	assert.Equal(t, want, s.reasoningContent.String())
	assert.Equal(t, want, s.details.String())
	assertNoPlaceholderLeak(t, out)

	for _, f := range s.frames {
		r, hasR := f["reasoning"]
		rc, hasRC := f["reasoning_content"]
		if !hasR && !hasRC {
			continue
		}
		assert.True(t, hasR && hasRC, "reasoning fields of one delta must share a frame: %v", f)
		assert.Equal(t, r, rc)
		details, _ := f["reasoning_details"].([]any)
		require.Len(t, details, 1, "the reasoning_details entry rides the same frame: %v", f)
		assert.Equal(t, r, details[0].(map[string]any)["text"])
	}

	var signatures int
	for _, f := range s.frames {
		details, _ := f["reasoning_details"].([]any)
		for _, d := range details {
			entry := d.(map[string]any)
			if entry["signature"] != nil {
				signatures++
				assert.Equal(t, "c2ln", entry["signature"], "the signature is relayed verbatim")
				assert.Equal(t, "anthropic-claude-v1", entry["format"])
			}
		}
	}
	assert.Equal(t, 1, signatures, "exactly one entry carries the signature")
}

// A signature ends its reasoning_details entry: whatever the demasker still
// holds goes out with it, and entries whose text is only buffered (and carry
// nothing else) are not emitted empty.
func TestReasoningDetails_SignatureFlushesHeldText(t *testing.T) {
	frames := []string{
		dataFrame(`{"reasoning_details":[{"type":"reasoning.text","text":"пер","format":"f1","index":0}]}`),
		dataFrame(`{"reasoning_details":[{"type":"reasoning.text","text":"вый","format":"f1","index":0}]}`),
		dataFrame(`{"reasoning_details":[{"type":"reasoning.text","text":"","signature":"sig","format":"f1","index":0}]}`),
	}
	out := feedFrames(t, New(bufferingDemasker()), frames, false)

	s := collectReasoningStream(t, out)
	require.Len(t, s.frames, 1, "buffered text-only entries are not emitted; the signed one carries the text")
	details := s.frames[0]["reasoning_details"].([]any)
	require.Len(t, details, 1)
	assert.Equal(t, map[string]any{
		"type": "reasoning.text", "text": "первый", "signature": "sig", "format": "f1", "index": float64(0),
	}, details[0])
}

// Entries without text (encrypted reasoning) pass through untouched, and a
// flush releases held text in an entry shaped after the last one seen for
// that index — without its signature or data.
func TestReasoningDetails_EncryptedPassthroughAndFlushTemplate(t *testing.T) {
	frames := []string{
		dataFrame(`{"reasoning_details":[{"type":"reasoning.summary","summary":"итог <FI","format":"f2","id":"r1","index":2}]}`),
		dataFrame(`{"reasoning_details":[{"type":"reasoning.encrypted","data":"ZW5j","format":"f2","id":"r1","index":3}]}`),
		dataFrame(`{"content":"ответ"}`),
		"data: [DONE]\n\n",
	}
	out := feedFrames(t, New(placeholdersDemasker(testOriginals)), frames, true)

	s := collectReasoningStream(t, out)
	var entries []map[string]any
	for _, f := range s.frames {
		details, _ := f["reasoning_details"].([]any)
		for _, d := range details {
			entries = append(entries, d.(map[string]any))
		}
	}
	require.Len(t, entries, 3)
	assert.Equal(t, map[string]any{"type": "reasoning.summary", "summary": "итог ", "format": "f2", "id": "r1", "index": float64(2)}, entries[0])
	assert.Equal(t, map[string]any{"type": "reasoning.encrypted", "data": "ZW5j", "format": "f2", "id": "r1", "index": float64(3)}, entries[1])
	assert.Equal(t, map[string]any{"type": "reasoning.summary", "summary": "<FI", "format": "f2", "id": "r1", "index": float64(2)}, entries[2],
		"the held tail goes out when content starts, in an entry built from the last one for index 2")
	assert.Equal(t, "ответ", s.content.String())
}

// Held reasoning_content is released at every point that ends reasoning:
// finish_reason, [DONE] and a stream that just stops.
func TestReasoningContent_FlushPoints(t *testing.T) {
	cases := map[string]struct {
		tail []string
		eos  bool
	}{
		"finish_reason": {tail: []string{`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"}},
		"done":          {tail: []string{"data: [DONE]\n\n"}},
		"eos":           {eos: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			frames := append([]string{
				dataFrame(`{"reasoning_content":"раз "}`),
				dataFrame(`{"reasoning_content":"два"}`),
			}, tc.tail...)
			out := feedFrames(t, New(bufferingDemasker()), frames, tc.eos)
			s := collectReasoningStream(t, out)
			assert.Equal(t, "раз два", s.reasoningContent.String())
		})
	}
}

func TestReasoningContent_MultiChoice(t *testing.T) {
	frame := func(idx, text string) string {
		return `data: {"id":"c1","choices":[{"index":` + idx + `,"delta":{"reasoning_content":` + jsonString(text) + `}}]}` + "\n\n"
	}
	frames := []string{frame("0", "первый <FI"), frame("1", "второй <PASS"), frame("0", "O_1>"), frame("1", "PORT_RF_1>"), "data: [DONE]\n\n"}
	out := feedFrames(t, New(placeholdersDemasker(testOriginals)), frames, true)

	got := map[int64]string{}
	all, _ := splitFrames(out)
	for _, f := range all {
		payload := strings.TrimPrefix(strings.TrimSpace(string(f)), "data: ")
		for _, c := range gjson.Get(payload, "choices").Array() {
			got[c.Get("index").Int()] += c.Get("delta.reasoning_content").String()
		}
	}
	assert.Equal(t, map[int64]string{0: "первый Иванов Иван Петрович", 1: "второй 4510 123456"}, got)
	assertNoPlaceholderLeak(t, out)
}
