package gateway

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/models"
)

// triggeredHeaders renders the outcome of a masked request as the response
// headers that report it, in the format of the ext_proc variant:
//
//   - data types (x-guardrails-data-types-triggered): the numeric IDs of the
//     triggered data types, ascending, comma-separated — "5" or "2,5";
//   - triggered rules (x-guardrails-triggered-rules): the IDs of the triggered
//     rules, ascending, comma-separated — "pii.docs.inn-person,pii.fio-ru";
//   - replacement counts (x-guardrails-replacement-counts): the same rules in
//     the same order as rule_id=count, where count is the number of distinct
//     values the rule replaced with a placeholder (a value repeated in the
//     request reuses its placeholder and counts once) —
//     "pii.docs.inn-person=1,pii.fio-ru=2".
//
// As in the ext_proc variant, the data-types header is always set; rule IDs
// reveal which detectors fired, so the two rule headers are opt-in
// (GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES). A header whose configured name
// is empty is left out.
func (h *Handler) triggeredHeaders(st models.MaskingState) http.Header {
	names := h.cfg.GuardrailsHeaders
	out := make(http.Header, 3)
	if len(st.TriggeredDataTypes) > 0 {
		setNamed(out, names.DataTypesHeader, strings.Join(dataTypeIDs(st.TriggeredDataTypes), ","))
	}
	if names.ExposeTriggeredRules && len(st.TriggeredRuleIDs) > 0 {
		setNamed(out, names.TriggeredRulesHeader, strings.Join(st.TriggeredRuleIDs, ","))
		setNamed(out, names.ReplacementCountsHeader, replacementCounts(st))
	}
	return out
}

// replacementCounts formats, for each triggered rule in order, the number of
// replacements attributed to it. A rule can trigger without a replacement of
// its own (its match was a value another rule had already replaced); it is
// listed with 0, so both rule headers always name the same rules.
func replacementCounts(st models.MaskingState) string {
	counts := make(map[string]int, len(st.TriggeredRuleIDs))
	for _, rep := range st.Replacements {
		counts[rep.RuleID]++
	}
	var b strings.Builder
	for i, id := range st.TriggeredRuleIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(id)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(counts[id]))
	}
	return b.String()
}

// setTriggeredHeaders makes the outcome headers this service reports on dst
// this request's own: values the upstream sent under those names are dropped
// (relayed, they would read as this service's verdict — including on a request
// it did not mask), then triggered is set. The data-types header is always
// this service's; the two rule headers are only while exposure is on, and
// otherwise the upstream's values under those names are relayed untouched.
func (h *Handler) setTriggeredHeaders(dst, triggered http.Header) {
	names := h.cfg.GuardrailsHeaders
	owned := [3]string{names.DataTypesHeader}
	if names.ExposeTriggeredRules {
		owned[1], owned[2] = names.TriggeredRulesHeader, names.ReplacementCountsHeader
	}
	for _, name := range owned {
		if name != "" {
			dst.Del(name)
		}
	}
	for name, values := range triggered {
		dst[name] = values
	}
}

// setNamed sets a header unless its configured name is empty (disabled).
func setNamed(h http.Header, name, value string) {
	if name != "" {
		h.Set(name, value)
	}
}
