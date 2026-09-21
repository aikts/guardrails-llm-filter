package responses

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/sseproc/common"
)

// summaryDelta is the delta LiteLLM emits for a chat model's
// reasoning_content: no sequence_number and no summary_index.
func summaryDelta(delta string) string {
	return frame("response.reasoning_summary_text.delta",
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":`+quote(delta)+`}`)
}

func quote(s string) string {
	b, _ := common.MarshalNoEscape(s)
	return strings.TrimSuffix(string(b), "\n")
}

type parsedEvent struct {
	name string
	data string
}

func parseEvents(t *testing.T, out string) []parsedEvent {
	t.Helper()
	frames, _ := common.SplitFrames([]byte(out))
	events := make([]parsedEvent, 0, len(frames))
	for _, f := range frames {
		pf := common.ClassifyFrame(f)
		require.Equal(t, common.FrameEvent, pf.Kind, "unexpected frame %q", f)
		require.True(t, gjson.ValidBytes(pf.Data), "frame must carry valid JSON: %q", pf.Data)
		events = append(events, parsedEvent{name: string(pf.Event), data: string(pf.Data)})
	}
	return events
}

// LiteLLM serves a chat model's reasoning_content on /v1/responses as
// reasoning summary events. The processor relayed them verbatim, so the
// placeholders the model echoed into its reasoning reached the client, while
// the output_item.done and response.completed snapshots were demasked.
func TestReasoningSummary_SplitPlaceholderDemasked(t *testing.T) {
	t.Parallel()
	p := New(bufferingReplacing())

	out := process(t, p, []string{
		createdFrame(),
		summaryDelta("Клиент <EMA"),
		summaryDelta("IL_1> пишет"),
		frame("response.reasoning_summary_text.done",
			`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"Клиент <EMAIL_1> пишет"}`),
		frame("response.reasoning_summary_part.done",
			`{"type":"response.reasoning_summary_part.done","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"Клиент <EMAIL_1> пишет"}}`),
		frame("response.output_item.done",
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Клиент <EMAIL_1> пишет"}]}}`),
		frame("response.completed",
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[`+
				`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Клиент <EMAIL_1> пишет"}]},`+
				`{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`),
	}, true)

	assert.NotContains(t, out, "<EMA")
	assert.NotContains(t, out, "IL_1>")

	const want = "Клиент user@example.com пишет"
	events := parseEvents(t, out)
	var deltas strings.Builder
	var sawDone bool
	for _, e := range events {
		switch gjson.Get(e.data, "type").String() {
		case "response.reasoning_summary_text.delta":
			assert.False(t, sawDone, "the held text goes out before the done event")
			assert.Equal(t, "response.reasoning_summary_text.delta", e.name)
			assert.Equal(t, "rs_1", gjson.Get(e.data, "item_id").String())
			assert.Equal(t, int64(0), gjson.Get(e.data, "summary_index").Int())
			assert.False(t, gjson.Get(e.data, "content_index").Exists(), "summaries are indexed by summary_index")
			deltas.WriteString(gjson.Get(e.data, "delta").String())
		case "response.reasoning_summary_text.done":
			sawDone = true
			assert.Equal(t, want, gjson.Get(e.data, "text").String())
		case "response.reasoning_summary_part.done":
			assert.Equal(t, want, gjson.Get(e.data, "part.text").String())
		case "response.output_item.done":
			assert.Equal(t, want, gjson.Get(e.data, "item.summary.0.text").String())
		case "response.completed":
			assert.Equal(t, want, gjson.Get(e.data, "response.output.0.summary.0.text").String())
		}
	}
	assert.Equal(t, want, deltas.String())
	assert.True(t, sawDone)
}

// LiteLLM serving a chat model on /v1/responses snapshots the reasoning in
// shapes of its own: a content_part.done whose part is
// {"type":"reasoning_text","reasoning":...}, and a reasoning output item
// whose content parts are typed output_text. Both repeat the full reasoning
// and leaked it with its placeholders (seen live).
func TestReasoning_LiteLLMSnapshotShapes(t *testing.T) {
	t.Parallel()
	p := New(replacing("<EMAIL_1>", "user@example.com"))

	out := process(t, p, []string{
		"data: " + `{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"reasoning_text","reasoning":"think <EMAIL_1>"}}` + "\n\n",
		"data: " + `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[` +
			`{"type":"reasoning","id":"rs_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"think <EMAIL_1>","annotations":[]}]},` +
			`{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}}` + "\n\n",
	}, true)

	assert.NotContains(t, out, "<EMAIL_1>")
	events := parseEvents(t, out)
	require.Len(t, events, 2)
	assert.Equal(t, "think user@example.com", gjson.Get(events[0].data, "part.reasoning").String())
	assert.Equal(t, "think user@example.com", gjson.Get(events[1].data, "response.output.0.content.0.text").String())
}

// Each summary part has its own demasker: finishing part 0 releases only
// what was held for part 0.
func TestReasoningSummary_PartsKeyedBySummaryIndex(t *testing.T) {
	t.Parallel()
	p := New(bufferingReplacing())
	delta := func(idx, text string) string {
		return frame("response.reasoning_summary_text.delta",
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":`+idx+`,"delta":`+quote(text)+`}`)
	}

	out := process(t, p, []string{
		delta("0", "первая <EMAIL_1>"),
		delta("1", "вторая"),
		frame("response.reasoning_summary_text.done",
			`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"первая <EMAIL_1>"}`),
	}, false)

	var released []string
	for _, e := range parseEvents(t, out) {
		if gjson.Get(e.data, "type").String() == "response.reasoning_summary_text.delta" {
			released = append(released, gjson.Get(e.data, "summary_index").String()+":"+gjson.Get(e.data, "delta").String())
		}
	}
	assert.Equal(t, []string{"0:первая user@example.com"}, released, "part 1 is still held")
}
