package llmutils

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// CollapseDuplicateKeys rewrites a JSON document so that no object in it
// repeats a key: each key stays where it first occurred and takes the value of
// its last occurrence — the object Python's json and orjson, JavaScript's
// JSON.parse and Go's encoding/json build from such a document.
//
// The extractors read request fields with gjson, which returns the FIRST
// occurrence of a key (and sjson patches the first one), while typical LLM
// backends keep the LAST. Without collapsing, a body such as
//
//	{"messages":[{"role":"user","content":"clean","content":"<PII>"}]}
//
// is scanned as "clean" and reaches the model with "<PII>" in it. After
// collapsing there is one value per key, so the scanned text and the text the
// upstream reads are the same.
//
// Keys are compared decoded ("\u0063ontent" repeats "content"). Only objects
// that repeat a key, and the containers around them, are re-serialized
// (compactly, keeping every key and scalar as written); the rest of the
// document is copied verbatim. Besides strict JSON, the NaN, Infinity and
// -Infinity literals that Python's json module accepts are allowed. It returns
// body unchanged and false when no key repeats or body is not such a document.
func CollapseDuplicateKeys(body []byte) ([]byte, bool) {
	doc := gjson.ParseBytes(body)
	if !doc.IsObject() && !doc.IsArray() {
		return body, false
	}
	if !hasDuplicateKeys(doc) {
		return body, false
	}
	// gjson's getters are lenient; never rebuild a document a strict parser
	// would reject, since the rebuilt text could then read differently.
	if !gjson.ValidBytes(body) && !gjson.ValidBytes(nonFiniteAsNumbers(body)) {
		return body, false
	}
	collapsed, _ := collapse(doc)
	return []byte(collapsed), true
}

// smallObject is the key count up to which an object's keys are compared by a
// linear scan instead of a map.
const smallObject = 16

// keySet tracks the decoded keys of one object.
type keySet struct {
	list []string
	set  map[string]struct{}
}

// add records key and reports whether it was already present.
func (s *keySet) add(key string) bool {
	if s.set != nil {
		if _, ok := s.set[key]; ok {
			return true
		}
		s.set[key] = struct{}{}
		return false
	}
	for _, seen := range s.list {
		if seen == key {
			return true
		}
	}
	s.list = append(s.list, key)
	if len(s.list) > smallObject {
		s.set = make(map[string]struct{}, 2*len(s.list))
		for _, k := range s.list {
			s.set[k] = struct{}{}
		}
	}
	return false
}

func hasDuplicateKeys(v gjson.Result) bool {
	found := false
	switch {
	case v.IsObject():
		var keys keySet
		v.ForEach(func(k, val gjson.Result) bool {
			found = keys.add(k.Str) || hasDuplicateKeys(val)
			return !found
		})
	case v.IsArray():
		v.ForEach(func(_, val gjson.Result) bool {
			found = hasDuplicateKeys(val)
			return !found
		})
	}
	return found
}

type member struct {
	key   string // raw, quoted
	value string // raw
}

// collapse returns v's text with duplicate keys collapsed at every depth, and
// whether that differs from v.Raw. An unchanged value is returned as written.
func collapse(v gjson.Result) (string, bool) {
	switch {
	case v.IsObject():
		var members []member
		at := make(map[string]int)
		changed := false
		v.ForEach(func(k, val gjson.Result) bool {
			value, valueChanged := collapse(val)
			changed = changed || valueChanged
			if i, seen := at[k.Str]; seen {
				members[i].value = value
				changed = true
				return true
			}
			at[k.Str] = len(members)
			members = append(members, member{key: k.Raw, value: value})
			return true
		})
		if !changed {
			return v.Raw, false
		}
		var b strings.Builder
		b.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(m.key)
			b.WriteByte(':')
			b.WriteString(m.value)
		}
		b.WriteByte('}')
		return b.String(), true
	case v.IsArray():
		var items []string
		changed := false
		v.ForEach(func(_, val gjson.Result) bool {
			item, itemChanged := collapse(val)
			changed = changed || itemChanged
			items = append(items, item)
			return true
		})
		if !changed {
			return v.Raw, false
		}
		return "[" + strings.Join(items, ",") + "]", true
	default:
		return v.Raw, false
	}
}

// nonFiniteAsNumbers returns a copy of body in which the NaN, Infinity and
// -Infinity literals outside strings are replaced by JSON numbers of the same
// length, so that a strict validator can check the rest of the document.
// A literal counts only in value position, so "1NaN" stays invalid.
func nonFiniteAsNumbers(body []byte) []byte {
	out := bytes.Clone(body)
	inString, escaped := false, false
	var prev byte // last non-space byte outside strings
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
				prev = c
			}
			continue
		case c == '"':
			inString = true
			continue
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			continue
		}
		if prev == ':' || prev == ',' || prev == '[' {
			for _, lit := range nonFiniteLiterals {
				if bytes.HasPrefix(out[i:], lit.text) && endsToken(out, i+len(lit.text)) {
					copy(out[i:], lit.number)
					i += len(lit.text) - 1
					c = '0'
					break
				}
			}
		}
		prev = c
	}
	return out
}

var nonFiniteLiterals = []struct{ text, number []byte }{
	{[]byte("NaN"), []byte("0e0")},
	{[]byte("Infinity"), []byte("0e000000")},
	{[]byte("-Infinity"), []byte("-0e000000")},
}

func endsToken(b []byte, i int) bool {
	if i >= len(b) {
		return true
	}
	switch b[i] {
	case ' ', '\t', '\n', '\r', ',', ']', '}':
		return true
	}
	return false
}
