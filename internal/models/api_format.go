package models

import (
	"fmt"
	"sort"
	"strings"
)

// APIFormat identifies the wire format of an LLM API endpoint. It drives
// request text extraction, full-response demasking and SSE processor
// selection; the raw request path is resolved to a format exactly once in
// the request-headers phase.
type APIFormat string

const (
	// APIFormatChatCompletions is the OpenAI /v1/chat/completions format.
	APIFormatChatCompletions APIFormat = "chat_completions"
	// APIFormatMessages is the Anthropic /v1/messages format.
	APIFormatMessages APIFormat = "messages"
	// APIFormatResponses is the OpenAI /v1/responses format.
	APIFormatResponses APIFormat = "responses"
)

// apiFormats lists every known format; keep in sync with the constants.
var apiFormats = []APIFormat{
	APIFormatChatCompletions,
	APIFormatMessages,
	APIFormatResponses,
}

// ParseAPIFormat parses a format name case-insensitively.
func ParseAPIFormat(s string) (APIFormat, error) {
	f := APIFormat(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range apiFormats {
		if f == known {
			return known, nil
		}
	}
	return "", fmt.Errorf("unknown api format %q (valid: %s)", s, joinAPIFormats())
}

func joinAPIFormats() string {
	names := make([]string, len(apiFormats))
	for i, f := range apiFormats {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// PathResolver maps request paths to API formats. Matching is exact first,
// then longest-suffix: a configured key `/v1/messages` also matches
// `/openai/v1/messages` (keys start with `/`, so suffix matches are
// segment-anchored), letting proxy-prefixed mounts work with zero extra
// configuration. Query strings are stripped before matching.
type PathResolver struct {
	exact      map[string]APIFormat
	suffixKeys []string // sorted by length desc, so the longest suffix wins
}

// NewPathResolver validates a path→format map (e.g. from GUARDRAILS_PATHS)
// and builds a resolver. Errors are configuration mistakes and must fail
// the boot: an empty or invalid map would silently reject all traffic.
func NewPathResolver(paths map[string]string) (*PathResolver, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("path map is empty: at least one path:format pair is required")
	}
	exact := make(map[string]APIFormat, len(paths))
	keys := make([]string, 0, len(paths))
	for path, format := range paths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \t") {
			return nil, fmt.Errorf("invalid path %q: must start with '/' and contain no whitespace", path)
		}
		f, err := ParseAPIFormat(format)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", path, err)
		}
		exact[path] = f
		keys = append(keys, path)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return &PathResolver{exact: exact, suffixKeys: keys}, nil
}

// Resolve maps a raw :path header value to its API format and to the
// CONFIGURED path that matched — the route, as opposed to the raw request path
// it was matched against. They differ for a suffix match (`/openai/v1/messages`
// resolves to the route `/v1/messages`), and only the route is drawn from
// configuration: the request path carries an arbitrary client-controlled
// prefix, so it is unfit for anything that expects a bounded set of values (a
// span name, a metric label). The last return is false, and the route empty,
// when no configured path matches.
func (r *PathResolver) Resolve(rawPath string) (APIFormat, string, bool) {
	path := rawPath
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if f, ok := r.exact[path]; ok {
		return f, path, true
	}
	for _, key := range r.suffixKeys {
		if len(path) > len(key) && strings.HasSuffix(path, key) {
			return r.exact[key], key, true
		}
	}
	return "", "", false
}
