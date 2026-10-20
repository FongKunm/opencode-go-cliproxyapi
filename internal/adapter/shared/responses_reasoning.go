package shared

// Responses reasoning output (FR-006): upstream reasoning text rendered as a
// "reasoning" output item whose text travels in one summary_text part.
//
// Summary rather than reasoning_text content: Codex renders reasoning
// summaries by default while raw reasoning_text stays hidden unless the
// user enables show_raw_agent_reasoning, so summary is what makes the
// thinking visible; it is also the shape CPA's own Chat
// Completions→Responses translator emits. No encrypted_content is set:
// there is nothing to encrypt, and replaying a reasoning item without it to
// a real OpenAI upstream is already sanitized host-side.

// RespReasoningItem is one synthesized reasoning output item. Summary is
// always a JSON array (never null): Codex requires the field when it
// decodes the item.
type RespReasoningItem struct {
	Type    string                 `json:"type"` // always "reasoning"
	ID      string                 `json:"id"`
	Summary []RespReasoningSummary `json:"summary"`
}

// RespReasoningSummary is one summary part of a reasoning item.
type RespReasoningSummary struct {
	Type string `json:"type"` // always "summary_text"
	Text string `json:"text"`
}

// NewRespReasoningItem builds a reasoning item carrying text as its single
// summary_text part; empty text yields an empty summary array.
func NewRespReasoningItem(id, text string) RespReasoningItem {
	summary := []RespReasoningSummary{}
	if text != "" {
		summary = append(summary, RespReasoningSummary{Type: "summary_text", Text: text})
	}
	return RespReasoningItem{Type: "reasoning", ID: id, Summary: summary}
}

// ReasoningItemAdded announces a reasoning item (empty summary) before its
// first summary delta.
func (e ResponsesEventEmitter) ReasoningItemAdded(outputIndex int, itemID string) []byte {
	return e.ItemAdded(outputIndex, map[string]any{
		"type": "reasoning", "id": itemID, "summary": []any{},
	})
}

// ReasoningSummaryPartAdded opens summary part 0 of the announced item.
func (e ResponsesEventEmitter) ReasoningSummaryPartAdded(itemID string, outputIndex int) []byte {
	return SSEEvent("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "item_id": itemID, "output_index": outputIndex,
		"summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""},
	})
}

// ReasoningSummaryTextDelta streams one fragment of summary part 0.
func (e ResponsesEventEmitter) ReasoningSummaryTextDelta(itemID string, outputIndex int, delta string) []byte {
	return SSEEvent("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": itemID, "output_index": outputIndex,
		"summary_index": 0, "delta": delta,
	})
}

// ReasoningSummaryTextDone completes the text of summary part 0.
func (e ResponsesEventEmitter) ReasoningSummaryTextDone(itemID string, outputIndex int, text string) []byte {
	return SSEEvent("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": itemID, "output_index": outputIndex,
		"summary_index": 0, "text": text,
	})
}

// ReasoningSummaryPartDone completes summary part 0.
func (e ResponsesEventEmitter) ReasoningSummaryPartDone(itemID string, outputIndex int, text string) []byte {
	return SSEEvent("response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "item_id": itemID, "output_index": outputIndex,
		"summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text},
	})
}
