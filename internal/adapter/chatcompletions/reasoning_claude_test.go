package chatcompletions

import (
	"strconv"
	"strings"
	"testing"
)

// ---- non-stream: reasoning_content → leading thinking block ----

func claudeContent(t *testing.T, body string) []map[string]any {
	t.Helper()
	m := mustConvert(t, "claude", body)
	raw, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("content not an array: %v", m)
	}
	out := make([]map[string]any, len(raw))
	for i, b := range raw {
		out[i] = b.(map[string]any)
	}
	return out
}

func assertThinkingBlock(t *testing.T, blk map[string]any, text string) {
	t.Helper()
	if blk["type"] != "thinking" || blk["thinking"] != text {
		t.Fatalf("thinking block = %v, want thinking %q", blk, text)
	}
	if sig, ok := blk["signature"]; !ok || sig != "" {
		t.Fatalf("thinking block must carry an empty signature: %v", blk)
	}
}

func TestConvertNonStreamClaudeReasoning(t *testing.T) {
	t.Run("reasoning precedes text", func(t *testing.T) {
		c := claudeContent(t, `{"id":"r","model":"deepseek-v4-pro","choices":[{"finish_reason":"stop",`+
			`"message":{"role":"assistant","content":"15","reasoning_content":"We need to add 7 and 8."}}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}`)
		if len(c) != 2 {
			t.Fatalf("want thinking + text, got %v", c)
		}
		assertThinkingBlock(t, c[0], "We need to add 7 and 8.")
		if c[1]["type"] != "text" || c[1]["text"] != "15" {
			t.Fatalf("text block wrong: %v", c[1])
		}
	})
	t.Run("reasoning precedes text and tool_use", func(t *testing.T) {
		m := mustConvert(t, "claude", `{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",`+
			`"message":{"content":"Let me look.","reasoning_content":"Need the file.",`+
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`)
		c := m["content"].([]any)
		if len(c) != 3 {
			t.Fatalf("want thinking + text + tool_use, got %v", c)
		}
		assertThinkingBlock(t, c[0].(map[string]any), "Need the file.")
		if c[1].(map[string]any)["type"] != "text" || c[2].(map[string]any)["type"] != "tool_use" {
			t.Fatalf("block order wrong: %v", c)
		}
		if m["stop_reason"] != "tool_use" {
			t.Fatalf("stop_reason = %v", m["stop_reason"])
		}
	})
	t.Run("reasoning with tool_use only", func(t *testing.T) {
		c := claudeContent(t, `{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",`+
			`"message":{"content":null,"reasoning_content":"Call it.",`+
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
		if len(c) != 2 || c[1]["type"] != "tool_use" {
			t.Fatalf("want thinking + tool_use, got %v", c)
		}
		assertThinkingBlock(t, c[0], "Call it.")
	})
	t.Run("reasoning only", func(t *testing.T) {
		c := claudeContent(t, `{"id":"r","model":"m","choices":[{"finish_reason":"length",`+
			`"message":{"content":"","reasoning_content":"Still thinking"}}]}`)
		if len(c) != 1 {
			t.Fatalf("want a lone thinking block, got %v", c)
		}
		assertThinkingBlock(t, c[0], "Still thinking")
	})
	t.Run("reasoning fallback field", func(t *testing.T) {
		c := claudeContent(t, `{"id":"r","model":"m","choices":[{"finish_reason":"stop",`+
			`"message":{"content":"ok","reasoning":"via reasoning"}}]}`)
		if len(c) != 2 {
			t.Fatalf("want thinking + text, got %v", c)
		}
		assertThinkingBlock(t, c[0], "via reasoning")
	})
	t.Run("reasoning_content wins over reasoning", func(t *testing.T) {
		c := claudeContent(t, `{"id":"r","model":"m","choices":[{"finish_reason":"stop",`+
			`"message":{"content":"ok","reasoning_content":"primary","reasoning":"primary"}}]}`)
		if len(c) != 2 {
			t.Fatalf("mirrored reasoning must not duplicate: %v", c)
		}
		assertThinkingBlock(t, c[0], "primary")
	})
	for name, body := range map[string]string{
		"null reasoning":       `{"id":"r","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok","reasoning_content":null}}]}`,
		"empty reasoning":      `{"id":"r","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok","reasoning_content":""}}]}`,
		"absent reasoning":     `{"id":"r","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`,
		"non-string reasoning": `{"id":"r","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok","reasoning":{"effort":"high"}}}]}`,
	} {
		t.Run(name+" leaves content unchanged", func(t *testing.T) {
			c := claudeContent(t, body)
			if len(c) != 1 || c[0]["type"] != "text" || c[0]["text"] != "ok" {
				t.Fatalf("content = %v, want the lone text block", c)
			}
		})
	}
}

// ---- stream: reasoning deltas → thinking block lifecycle ----

// blockLog summarizes a Messages event stream as "start:<i>:<type>",
// "delta:<i>:<type>", and "stop:<i>" entries (deltas of the same type and
// index collapse) so tests can pin exact block order.
func blockLog(evs []sseEvt) []string {
	var out []string
	for _, e := range evs {
		idx, _ := e.Data["index"].(float64)
		switch e.Name {
		case "content_block_start":
			out = append(out, "start:"+itoa(idx)+":"+e.Data["content_block"].(map[string]any)["type"].(string))
		case "content_block_delta":
			entry := "delta:" + itoa(idx) + ":" + e.Data["delta"].(map[string]any)["type"].(string)
			if len(out) == 0 || out[len(out)-1] != entry {
				out = append(out, entry)
			}
		case "content_block_stop":
			out = append(out, "stop:"+itoa(idx))
		default:
			out = append(out, e.Name)
		}
	}
	return out
}

func itoa(f float64) string { return strconv.Itoa(int(f)) }

func thinkingText(evs []sseEvt, index float64) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Name != "content_block_delta" || e.Data["index"] != index {
			continue
		}
		if d := e.Data["delta"].(map[string]any); d["type"] == "thinking_delta" {
			b.WriteString(d["thinking"].(string))
		}
	}
	return b.String()
}

func assertLog(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("event log mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestStreamConverterClaudeReasoningThenText(t *testing.T) {
	sc := NewStreamConverter("claude")
	evs := feedAll(t, sc,
		`data: {"id":"r1","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""}}],"usage":{"prompt_tokens":7}}`,
		`data: {"choices":[{"delta":{"content":null,"reasoning_content":"We need"}}]}`,
		`data: {"choices":[{"delta":{"content":null,"reasoning_content":" to add."}}]}`,
		`data: {"choices":[{"delta":{"content":"15","reasoning_content":null}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}`,
		`data: [DONE]`)
	assertLog(t, blockLog(evs), []string{
		"message_start",
		"start:0:thinking", "delta:0:thinking_delta", "stop:0",
		"start:1:text", "delta:1:text_delta", "stop:1",
		"message_delta", "message_stop",
	})
	assertCloseBeforeStart(t, evs)
	if got := thinkingText(evs, 0); got != "We need to add." {
		t.Fatalf("thinking deltas = %q", got)
	}
	start := evs[1].Data["content_block"].(map[string]any)
	if start["thinking"] != "" || start["signature"] != "" {
		t.Fatalf("thinking content_block_start must carry empty thinking and signature: %v", start)
	}
	md := evs[len(evs)-2].Data
	if md["delta"].(map[string]any)["stop_reason"] != "end_turn" ||
		md["usage"].(map[string]any)["output_tokens"] != float64(9) {
		t.Fatalf("terminal message_delta wrong: %v", md)
	}
}

func TestStreamConverterClaudeReasoningThenTools(t *testing.T) {
	sc := NewStreamConverter("claude")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant","reasoning_content":"Read both"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":" files."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"Read","arguments":"{\"p\":2}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	assertLog(t, blockLog(evs), []string{
		"message_start",
		"start:0:thinking", "delta:0:thinking_delta", "stop:0",
		"start:1:tool_use", "delta:1:input_json_delta", "stop:1",
		"start:2:tool_use", "delta:2:input_json_delta", "stop:2",
		"message_delta", "message_stop",
	})
	assertCloseBeforeStart(t, evs)
	if got := thinkingText(evs, 0); got != "Read both files." {
		t.Fatalf("thinking deltas = %q", got)
	}
	if evs[len(evs)-2].Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason must stay tool_use: %v", evs[len(evs)-2])
	}
}

// Reasoning that resumes after text (or a tool call) opens a fresh
// thinking block at the next index instead of reusing the closed one.
func TestStreamConverterClaudeReasoningResumes(t *testing.T) {
	sc := NewStreamConverter("claude")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning_content":"a"}}]}`,
		`data: {"choices":[{"delta":{"content":"x"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"b"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"c","content":"y"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	assertLog(t, blockLog(evs), []string{
		"message_start",
		"start:0:thinking", "delta:0:thinking_delta", "stop:0",
		"start:1:text", "delta:1:text_delta", "stop:1",
		"start:2:thinking", "delta:2:thinking_delta", "stop:2",
		"start:3:tool_use", "delta:3:input_json_delta", "stop:3",
		"start:4:thinking", "delta:4:thinking_delta", "stop:4",
		"start:5:text", "delta:5:text_delta", "stop:5",
		"message_delta", "message_stop",
	})
	assertCloseBeforeStart(t, evs)
	for idx, want := range map[float64]string{0: "a", 2: "b", 4: "c"} {
		if got := thinkingText(evs, idx); got != want {
			t.Fatalf("thinking block %v = %q, want %q", idx, got, want)
		}
	}
}

func TestStreamConverterClaudeReasoningOnlyAndFlush(t *testing.T) {
	sc := NewStreamConverter("claude")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	evs = append(evs, parseEvents(t, sc.Flush())...)
	assertLog(t, blockLog(evs), []string{
		"message_start",
		"start:0:thinking", "delta:0:thinking_delta", "stop:0",
		"message_delta", "message_stop",
	})
	if evs[len(evs)-2].Data["delta"].(map[string]any)["stop_reason"] != "max_tokens" {
		t.Fatalf("stop_reason wrong: %v", evs[len(evs)-2])
	}
}

func TestStreamConverterClaudeReasoningFieldVariants(t *testing.T) {
	t.Run("reasoning fallback field", func(t *testing.T) {
		sc := NewStreamConverter("claude")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning":"via"}}]}`,
			`data: {"choices":[{"delta":{"reasoning":" fallback"}}]}`,
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`data: [DONE]`)
		if got := thinkingText(evs, 0); got != "via fallback" {
			t.Fatalf("fallback reasoning = %q (%v)", got, blockLog(evs))
		}
	})
	t.Run("mirrored fields do not duplicate", func(t *testing.T) {
		sc := NewStreamConverter("claude")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning_content":"one","reasoning":"one"}}]}`,
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`data: [DONE]`)
		if got := thinkingText(evs, 0); got != "one" {
			t.Fatalf("mirrored reasoning = %q", got)
		}
	})
	t.Run("non-string reasoning is ignored", func(t *testing.T) {
		sc := NewStreamConverter("claude")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning":{"summary":"x"},"reasoning_content":7}}]}`,
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`data: [DONE]`)
		assertLog(t, blockLog(evs), []string{
			"message_start", "start:0:text", "delta:0:text_delta", "stop:0", "message_delta", "message_stop",
		})
	})
}

// Without reasoning the event stream must be byte-identical to a stream
// whose chunks never mention reasoning fields at all.
func TestStreamConverterClaudeNoReasoningUnchanged(t *testing.T) {
	plain := []string{
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`data: [DONE]`,
	}
	nulls := []string{
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant","reasoning_content":null}}]}`,
		`data: {"choices":[{"delta":{"content":"hi","reasoning_content":""}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}],"reasoning_content":null}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		`data: [DONE]`,
	}
	raw := func(lines []string) string {
		sc := NewStreamConverter("claude")
		var b strings.Builder
		for _, l := range lines {
			evs, _, eErr := sc.Feed([]byte(l + "\n"))
			if eErr != nil {
				t.Fatalf("feed: %v", eErr)
			}
			for _, e := range evs {
				b.Write(e)
			}
		}
		return b.String()
	}
	if a, b := raw(plain), raw(nulls); a != b {
		t.Fatalf("empty reasoning fields changed the stream:\n%s\n----\n%s", a, b)
	}
	if strings.Contains(raw(plain), "thinking") {
		t.Fatal("plain stream must not mention thinking")
	}
}
