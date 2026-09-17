package llmutils

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bs writes JSON escapes in test bodies as "#": a literal backslash would need
// escaping twice over, which hides what the body actually contains.
func bs(s string) string { return strings.ReplaceAll(s, "#", string(rune(92))) }

func TestCollapseDuplicateKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string // empty: body is returned unchanged
	}{
		{
			name: "no duplicates is left as is",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "same key in different objects is not a duplicate",
			body: `{"content":"a","messages":[{"content":"b"},{"content":"c"}]}`,
		},
		{
			name: "repeated message content keeps the last value in the first position",
			body: `{"model":"m","messages":[{"role":"user","content":"clean","content":"PII"}]}`,
			want: `{"model":"m","messages":[{"role":"user","content":"PII"}]}`,
		},
		{
			name: "repeated top-level key",
			body: `{"messages":[{"role":"user","content":"clean"}],"model":"m","messages":[{"role":"user","content":"PII"}]}`,
			want: `{"messages":[{"role":"user","content":"PII"}],"model":"m"}`,
		},
		{
			name: "repeated part type",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","type":"text","text":"PII"}]}]}`,
			want: `{"messages":[{"role":"user","content":[{"type":"text","text":"PII"}]}]}`,
		},
		{
			name: "keys are compared decoded",
			body: bs(`{"messages":[{"content":"clean","role":"user","#u0063ontent":"PII"}]}`),
			want: `{"messages":[{"content":"PII","role":"user"}]}`,
		},
		{
			name: "a key repeated three times keeps the last value",
			body: `{"a":1,"b":2,"a":3,"a":4}`,
			want: `{"a":4,"b":2}`,
		},
		{
			name: "values without duplicates keep their formatting and escapes",
			body: bs(`{ "tools" : [ {"name" : "f", "d" : "q#"x"} ], "n": 1.50e+3, "messages":[{"content":"x","content":"y"}] }`),
			want: bs(`{"tools":[ {"name" : "f", "d" : "q#"x"} ],"n":1.50e+3,"messages":[{"content":"y"}]}`),
		},
		{
			name: "duplicates inside an array at the top level",
			body: `[{"a":1,"a":2},{"b":3}]`,
			want: `[{"a":2},{"b":3}]`,
		},
		{
			name: "many keys switch to a set",
			body: `{"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k17":17,"k18":18,"k1":0}`,
			want: `{"k1":0,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k17":17,"k18":18}`,
		},
		{
			name: "non-finite literals Python accepts are kept",
			body: `{"temperature":NaN,"top_p":[Infinity, -Infinity],"messages":[{"content":"clean","content":"PII"}]}`,
			want: `{"temperature":NaN,"top_p":[Infinity, -Infinity],"messages":[{"content":"PII"}]}`,
		},
		{
			name: "truncated document is left as is",
			body: `{"messages":[{"content":"clean","content":"PII"}]`,
		},
		{
			name: "garbage in a dropped duplicate does not make the document valid",
			body: `{"a":1NaN,"a":2,"messages":[{"content":"PII"}]}`,
		},
		{
			name: "trailing data is left as is",
			body: `{"a":1,"a":2} {}`,
		},
		{
			name: "scalar document is left as is",
			body: `"text"`,
		},
		{
			name: "empty body is left as is",
			body: ``,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, changed := CollapseDuplicateKeys([]byte(tt.body))
			if tt.want == "" {
				assert.False(t, changed)
				assert.Equal(t, tt.body, string(got))
				return
			}
			require.True(t, changed)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

// TestCollapseDuplicateKeysMatchesLastWinsParser checks the collapsed document
// against encoding/json, which, like Python's json, keeps a repeated key's last
// value.
func TestCollapseDuplicateKeysMatchesLastWinsParser(t *testing.T) {
	t.Parallel()

	body := bs(`{"model":"m","stream":false,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"},"type":"text","text":"a"}],"content":"b#u0022"}],"stream":true,"m#u006fdel":"n"}`)
	var want any
	require.NoError(t, json.Unmarshal([]byte(body), &want))

	got, changed := CollapseDuplicateKeys([]byte(body))
	require.True(t, changed)
	var parsed any
	require.NoError(t, json.Unmarshal(got, &parsed))
	assert.Equal(t, want, parsed)
}

func TestNonFiniteAsNumbers(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		`{"a":NaN}`:                      `{"a":0e0}`,
		`[Infinity,-Infinity]`:           `[0e000000,-0e000000]`,
		`{"a": [ NaN ]}`:                 `{"a": [ 0e0 ]}`,
		`{"NaN":"NaN"}`:                  `{"NaN":"NaN"}`,
		bs(`{"a":"#",NaN"}`):             bs(`{"a":"#",NaN"}`),
		bs(`{"a":"##","b":NaN}`):         bs(`{"a":"##","b":0e0}`),
		`{"a":1NaN}`:                     `{"a":1NaN}`,
		`{"a":NaNx}`:                     `{"a":NaNx}`,
		`{"a":-NaN}`:                     `{"a":-NaN}`,
		`{"a":Infinityy,"b":-Infinity }`: `{"a":Infinityy,"b":-0e000000 }`,
	}
	for in, want := range tests {
		assert.Equal(t, want, string(nonFiniteAsNumbers([]byte(in))), "input %s", in)
	}
}
