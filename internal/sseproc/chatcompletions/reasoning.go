package chatcompletions

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/logging"
	llmchat "github.com/cloud-ru-tech/guardrails-llm-filter/pkg/llmutils/chatcompletions"
)

// A delta can carry the model's reasoning under several names at once —
// LiteLLM relaying OpenRouter sends the same text as "reasoning",
// "reasoning_content" and a "reasoning_details" entry. Each field has its own
// demasker, but whatever they release for one upstream delta goes out in ONE
// frame: split into a frame per field, a client that reads
// `delta.reasoning_content or delta.reasoning` would show the text twice.

// reasoningDetailTextFields are the reasoning_details entry fields that carry
// model text: "text" of a reasoning.text entry, "summary" of a
// reasoning.summary one. An entry has at most one of them.
var reasoningDetailTextFields = []string{"text", "summary"}

// reasoningDetailIdentity are the reasoning_details entry fields that only
// identify the entry; everything else (signature, encrypted data, fields a
// provider adds later) is payload the client must get verbatim.
var reasoningDetailIdentity = map[string]bool{"type": true, "index": true, "format": true, "id": true}

// reasoningFrame is what the reasoning demaskers of one choice released
// together; empty fields are left out of the frame.
type reasoningFrame struct {
	reasoning        string
	reasoningContent string
	details          []json.RawMessage
}

// processReasoning demasks every reasoning field of one choice's delta and
// outputs whatever is ready as a single frame.
func (p *Processor) processReasoning(ctx context.Context, choice llmchat.ChunkChoice) {
	idx := choice.Index
	reasoning := ptrVal(choice.Delta.Reasoning)
	reasoningContent := ptrVal(choice.Delta.ReasoningContent)

	if p.captureMasked {
		// The fields repeat one text; record it once.
		if reasoning != "" {
			p.masked.Add(strconv.Itoa(idx)+"/"+string(fieldReasoning), reasoning)
		} else {
			p.masked.Add(strconv.Itoa(idx)+"/"+string(fieldReasoning), reasoningContent)
		}
	}

	var out reasoningFrame
	if reasoning != "" {
		out.reasoning = p.demaskText(ctx, demaskerKey{idx, 0, fieldReasoning}, reasoning, false)
	}
	if reasoningContent != "" {
		out.reasoningContent = p.demaskText(ctx, demaskerKey{idx, 0, fieldReasoningContent}, reasoningContent, false)
	}
	for pos, entry := range choice.Delta.ReasoningDetails {
		out.details = append(out.details, p.demaskReasoningDetail(ctx, idx, pos, entry)...)
	}
	p.outputReasoningFrame(ctx, idx, out)
}

// demaskReasoningDetail demasks the text of one reasoning_details entry and
// returns the entries to output in its place: none while its text is
// buffered and it carries nothing else, the entry itself with the demasked
// text otherwise. A signature ends the entry's text, so the demasker is
// flushed and everything it held goes out ahead of (or with) the signature.
func (p *Processor) demaskReasoningDetail(ctx context.Context, choiceIdx, pos int, entry json.RawMessage) []json.RawMessage {
	detailIdx := pos
	if v := gjson.GetBytes(entry, "index"); v.Type == gjson.Number {
		detailIdx = int(v.Int())
	}
	key := demaskerKey{choiceIdx, detailIdx, fieldReasoningDetail}
	signed := gjson.GetBytes(entry, "signature").String() != ""

	path, text := reasoningDetailText(entry)
	if path == "" {
		// No text to demask (encrypted data, a bare signature): relay it as
		// is, after the text still held for this entry if it closes it.
		var out []json.RawMessage
		if signed {
			if tail := p.flushText(ctx, key); tail != "" {
				out = append(out, p.aggr.reasoningDetailTail(choiceIdx, detailIdx, tail))
			}
		}
		return append(out, entry)
	}

	p.aggr.rememberReasoningDetail(choiceIdx, detailIdx, path, entry)
	demasked := p.demaskText(ctx, key, text, signed)
	if demasked == "" && !reasoningDetailHasPayload(entry, path) {
		return nil
	}
	patched, err := sjson.SetBytes(entry, path, demasked)
	if err != nil {
		// Never fall back to the masked entry: its text holds placeholders.
		logging.Error(ctx, "Failed to patch reasoning_details entry", err, "choiceIdx", choiceIdx)
		return nil
	}
	return []json.RawMessage{patched}
}

// reasoningDetailText returns the text-carrying field of a reasoning_details
// entry and its value, or "" when the entry has none.
func reasoningDetailText(entry json.RawMessage) (path, text string) {
	for _, f := range reasoningDetailTextFields {
		if v := gjson.GetBytes(entry, f); v.Type == gjson.String {
			return f, v.String()
		}
	}
	return "", ""
}

// reasoningDetailHasPayload reports whether the entry carries anything
// besides its identity and its text — a signature or data the client needs
// even when the text itself is still buffered.
func reasoningDetailHasPayload(entry json.RawMessage, textPath string) bool {
	found := false
	gjson.ParseBytes(entry).ForEach(func(k, v gjson.Result) bool {
		name := k.String()
		if name == textPath || reasoningDetailIdentity[name] {
			return true
		}
		if v.Type == gjson.Null || (v.Type == gjson.String && v.String() == "") {
			return true
		}
		found = true
		return false
	})
	return found
}

// flushReasoning flushes every reasoning demasker of a choice and outputs
// what they held as a single frame.
func (p *Processor) flushReasoning(ctx context.Context, choiceIdx int) {
	var out reasoningFrame
	out.reasoning = p.flushText(ctx, demaskerKey{choiceIdx, 0, fieldReasoning})
	out.reasoningContent = p.flushText(ctx, demaskerKey{choiceIdx, 0, fieldReasoningContent})

	var detailIndices []int
	for key := range p.demaskers {
		if key.choiceIndex == choiceIdx && key.field == fieldReasoningDetail {
			detailIndices = append(detailIndices, key.toolCallIndex)
		}
	}
	slices.Sort(detailIndices)
	for _, detailIdx := range detailIndices {
		if tail := p.flushText(ctx, demaskerKey{choiceIdx, detailIdx, fieldReasoningDetail}); tail != "" {
			out.details = append(out.details, p.aggr.reasoningDetailTail(choiceIdx, detailIdx, tail))
		}
	}
	p.outputReasoningFrame(ctx, choiceIdx, out)
}

// outputReasoningFrame outputs one frame carrying every reasoning field that
// has something to show; nothing when none has.
func (p *Processor) outputReasoningFrame(ctx context.Context, choiceIdx int, f reasoningFrame) {
	if f.reasoning == "" && f.reasoningContent == "" && len(f.details) == 0 {
		return
	}
	p.outputDeltaFrame(ctx, choiceIdx, func(d *llmchat.Delta) {
		if f.reasoning != "" {
			d.Reasoning = &f.reasoning
		}
		if f.reasoningContent != "" {
			d.ReasoningContent = &f.reasoningContent
		}
		d.ReasoningDetails = f.details
	})
}
