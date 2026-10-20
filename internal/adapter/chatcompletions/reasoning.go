package chatcompletions

import (
	"encoding/json"
	"strconv"
	"strings"

	"opencode-go-cliproxyapi/internal/adapter/shared"
)

// ---- upstream reasoning text (FR-006) ----

// ccReasoning captures the reasoning text Chat Completions upstreams attach
// beside content: DeepSeek (and most OpenAI-compatible reasoning servers)
// send reasoning_content, others reasoning. Embedded in both the non-stream
// message and the stream delta. Both fields decode as raw JSON so an
// unexpected shape (object, array, number) is ignored instead of failing
// the whole response or chunk; only string values carry text.
type ccReasoning struct {
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
}

// reasoningText returns the reasoning text of one message or delta:
// reasoning_content wins, reasoning is the fallback (never both, so a
// server mirroring one into the other cannot duplicate text); "" when
// neither holds a non-empty string.
func (r ccReasoning) reasoningText() string {
	if s := rawString(r.ReasoningContent); s != "" {
		return s
	}
	return rawString(r.Reasoning)
}

// rawString decodes a raw JSON string value; null and non-string values
// yield "".
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// reasoningState is the per-stream bookkeeping for rendering upstream
// reasoning (embedded in StreamConverter).
type reasoningState struct {
	thinkingOpen  bool // claude: a thinking content block is open
	thinkingIndex int  // claude: index of the open thinking block

	rsOpen  *respReasoning   // openai-response: reasoning item still streaming
	rsItems []*respReasoning // openai-response: every announced reasoning item, in index order
}

// respReasoning is one streamed reasoning output item (openai-response).
type respReasoning struct {
	id    string
	index int // output_index assigned at announcement
	text  strings.Builder
}

// ---- claude (Anthropic Messages) target ----

// claudeThinkingBlock renders upstream reasoning as an Anthropic thinking
// block placed before text and tool_use blocks. The signature is empty:
// there is no upstream signature to relay, and CPA drops thinking blocks
// whose signature is invalid (empty included) before replaying history to
// a real Anthropic upstream, so the block can never pass for signed
// Anthropic thinking.
func claudeThinkingBlock(text string) claudeBlock {
	return claudeBlock{"type": "thinking", "thinking": text, "signature": ""}
}

// claudeReasoning renders one streamed reasoning fragment. The first
// fragment after any other block closes every open block (Anthropic
// forbids two concurrently open blocks) and opens a thinking block at the
// next index — content_block_start carries an empty thinking and an empty
// signature like Anthropic's own frames — and every fragment streams as a
// thinking_delta. Reasoning that resumes after text or tool_use opens a
// fresh thinking block at a new index.
func (sc *StreamConverter) claudeReasoning(text string) [][]byte {
	var events [][]byte
	if !sc.thinkingOpen {
		events = append(events, sc.stopBlocks()...)
		sc.thinkingIndex = sc.nextIndex
		sc.nextIndex++
		sc.thinkingOpen = true
		events = append(events, sc.claudeEm.ContentBlockStart(sc.thinkingIndex, "thinking",
			map[string]any{"thinking": "", "signature": ""}))
	}
	return append(events, sc.claudeEm.ContentBlockDelta(sc.thinkingIndex,
		map[string]any{"type": "thinking_delta", "thinking": text}))
}

// stopThinking closes the open thinking block, if any; stopBlocks calls it
// so every content_block_start, finish_reason, and [DONE] closes it.
func (sc *StreamConverter) stopThinking() [][]byte {
	if !sc.thinkingOpen {
		return nil
	}
	sc.thinkingOpen = false
	return [][]byte{sc.claudeEm.ContentBlockStop(sc.thinkingIndex)}
}

// ---- openai-response (Responses) target ----

// reasoningItemID names the n-th reasoning item of a response: "rs_<id>"
// for the first, "rs_<id>_<n>" after, so it never collides with the
// message item (which carries the bare response id) or a sibling.
func reasoningItemID(responseID string, n int) string {
	if n == 0 {
		return "rs_" + responseID
	}
	return "rs_" + responseID + "_" + strconv.Itoa(n)
}

// reasoningOutputItem renders non-stream reasoning as the leading output
// item (FR-006, chatToResponses); ok is false without reasoning text.
func reasoningOutputItem(responseID, text string) (shared.RespReasoningItem, bool) {
	if text == "" {
		return shared.RespReasoningItem{}, false
	}
	return shared.NewRespReasoningItem(reasoningItemID(responseID, 0), text), true
}

// responsesReasoning renders one streamed reasoning fragment as a
// Responses reasoning item (summary form, see shared.RespReasoningItem):
// the first fragment announces the item at the next output index
// (output_item.added with an empty summary, then summary part 0), and
// every fragment streams as response.reasoning_summary_text.delta.
// Reasoning that resumes after another item opens a fresh reasoning item.
func (sc *StreamConverter) responsesReasoning(text string) [][]byte {
	em := sc.responsesEm()
	var events [][]byte
	if sc.rsOpen == nil {
		r := &respReasoning{id: reasoningItemID(sc.id, len(sc.rsItems)), index: sc.nextIndex}
		sc.nextIndex++
		sc.rsOpen = r
		sc.rsItems = append(sc.rsItems, r)
		events = append(events, em.ReasoningItemAdded(r.index, r.id), em.ReasoningSummaryPartAdded(r.id, r.index))
	}
	sc.rsOpen.text.WriteString(text)
	return append(events, em.ReasoningSummaryTextDelta(sc.rsOpen.id, sc.rsOpen.index, text))
}

// closeReasoning completes the streaming reasoning item, if any: summary
// text done, summary part done, then output_item.done with the full item.
// It runs before any content or tool-call delta (items complete in order,
// so a client never sees the next item start while reasoning is still
// open) and at the terminal.
func (sc *StreamConverter) closeReasoning() [][]byte {
	r := sc.rsOpen
	if r == nil {
		return nil
	}
	sc.rsOpen = nil
	em := sc.responsesEm()
	text := r.text.String()
	return [][]byte{
		em.ReasoningSummaryTextDone(r.id, r.index, text),
		em.ReasoningSummaryPartDone(r.id, r.index, text),
		em.ItemDone(r.index, shared.NewRespReasoningItem(r.id, text)),
	}
}

// withReasoningItems merges the reasoning items into the terminal output
// array at their announced output indexes. items is the assembler's
// rendering of the message and function_call items in announcement order;
// every announced index renders exactly one item, so filling the
// non-reasoning slots in order reproduces the streamed indexes (the W4
// invariant: terminal output order equals announcement order).
func (sc *StreamConverter) withReasoningItems(items []any) []any {
	if len(sc.rsItems) == 0 {
		return items
	}
	out := make([]any, 0, len(items)+len(sc.rsItems))
	rs := sc.rsItems
	for len(rs) > 0 || len(items) > 0 {
		if len(rs) > 0 && (rs[0].index <= len(out) || len(items) == 0) {
			out = append(out, shared.NewRespReasoningItem(rs[0].id, rs[0].text.String()))
			rs = rs[1:]
			continue
		}
		out = append(out, items[0])
		items = items[1:]
	}
	return out
}
