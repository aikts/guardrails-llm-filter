package responses

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// LiteLLM streams /v1/responses as unnamed events (data lines only). The
// frames the processor rewrites or makes up must stay unnamed too — not gain
// an empty "event: " line — while a named stream keeps its names.
func TestEventNames_FollowTheStream(t *testing.T) {
	t.Parallel()
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m1","sequence_number":1,"delta":"to <EMA"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m1","sequence_number":2,"delta":"IL_1>"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"m1","sequence_number":3,"text":"to <EMAIL_1>"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"item_id":"m1","sequence_number":4,"part":{"type":"output_text","text":"to <EMAIL_1>"}}`,
		`{"type":"response.output_item.done","output_index":0,"sequence_number":5,"item":{"type":"message","content":[{"type":"output_text","text":"to <EMAIL_1>"}]}}`,
		`{"type":"response.completed","sequence_number":6,"response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"to <EMAIL_1>"}]}]}}`,
	}

	for _, named := range []bool{false, true} {
		var in []string
		for _, e := range events {
			if named {
				in = append(in, frame(gjson.Get(e, "type").String(), e))
			} else {
				in = append(in, "data: "+e+"\n\n")
			}
		}
		out := process(t, New(bufferingReplacing()), in, true)
		assert.NotContains(t, out, "<EMAIL_1>")

		parsed := parseEvents(t, out)
		require.Len(t, parsed, len(events)-1, "the two held deltas give way to the one the flush makes up")
		assert.Equal(t, "to user@example.com", gjson.Get(parsed[1].data, "delta").String(), "the made-up delta")
		for _, e := range parsed {
			if named {
				assert.Equal(t, gjson.Get(e.data, "type").String(), e.name)
			} else {
				assert.Empty(t, e.name, "an unnamed stream stays unnamed: %s", e.data)
			}
		}
		if !named {
			assert.NotContains(t, out, "event:", "no event line at all, empty or not")
		}
	}
}
