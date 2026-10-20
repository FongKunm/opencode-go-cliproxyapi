package chatcompletions

import "encoding/json"

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
