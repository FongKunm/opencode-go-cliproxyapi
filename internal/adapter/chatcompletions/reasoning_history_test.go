package chatcompletions

import "testing"

func buildMessages(t *testing.T, upstreamModel, sourceFormat, body string) []map[string]any {
	t.Helper()
	out, eErr := BuildRequest(upstreamModel, sourceFormat, []byte(body), nil)
	m := decodeOut(t, out, eErr)
	raw := m["messages"].([]any)
	msgs := make([]map[string]any, len(raw))
	for i, r := range raw {
		msgs[i] = r.(map[string]any)
	}
	return msgs
}

const claudeThinkingHistory = `{"max_tokens":64,"messages":[
	{"role":"user","content":"add 7 and 8, then read f"},
	{"role":"assistant","content":[
		{"type":"thinking","thinking":"We need the sum.","signature":""},
		{"type":"redacted_thinking","data":"enc"},
		{"type":"thinking","thinking":"Then read f.","signature":"sig-from-claude"},
		{"type":"text","text":"15."},
		{"type":"tool_use","id":"t1","name":"Read","input":{"p":"f"}}
	]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},
	{"role":"assistant","content":[{"type":"thinking","thinking":"   "},{"type":"text","text":"done"}]}
]}`

func TestClaudeHistoryThinkingPassedBackToDeepSeek(t *testing.T) {
	msgs := buildMessages(t, "deepseek-v4-pro", "claude", claudeThinkingHistory)
	if len(msgs) != 4 {
		t.Fatalf("want user/assistant/tool/assistant, got %v", msgs)
	}
	a := msgs[1]
	if a["role"] != "assistant" || a["content"] != "15." || len(a["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant turn wrong: %v", a)
	}
	// Every thinking block's text, signed or not, in order; redacted dropped.
	if a["reasoning_content"] != "We need the sum.\n\nThen read f." {
		t.Fatalf("reasoning_content = %q", a["reasoning_content"])
	}
	if _, ok := msgs[3]["reasoning_content"]; ok {
		t.Fatalf("blank thinking must not produce reasoning_content: %v", msgs[3])
	}
	for _, m := range []map[string]any{msgs[0], msgs[2]} {
		if _, ok := m["reasoning_content"]; ok {
			t.Fatalf("only assistant messages carry reasoning_content: %v", m)
		}
	}
}

func TestClaudeHistoryThinkingDroppedForOtherFamilies(t *testing.T) {
	for _, model := range []string{"kimi-k2", "glm-5.3", "m"} {
		for _, m := range buildMessages(t, model, "claude", claudeThinkingHistory) {
			if _, ok := m["reasoning_content"]; ok {
				t.Fatalf("%s: reasoning_content must stay omitted: %v", model, m)
			}
		}
	}
}

func TestClaudeHistoryThinkingOnlyAssistantStillDropped(t *testing.T) {
	msgs := buildMessages(t, "deepseek-v4-pro", "claude",
		`{"max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]}`)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("a thinking-only assistant turn has no answer to replay: %v", msgs)
	}
}

const responsesReasoningHistory = `{"input":[
	{"type":"message","role":"user","content":"add then read"},
	{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Need the file."}],"encrypted_content":null},
	{"type":"function_call","call_id":"c1","name":"Read","arguments":"{}"},
	{"type":"function_call","call_id":"c2","name":"Read","arguments":"{}"},
	{"type":"function_call_output","call_id":"c1","output":"a"},
	{"type":"function_call_output","call_id":"c2","output":"b"},
	{"type":"reasoning","summary":[{"type":"summary_text","text":"digest"}],"content":[{"type":"reasoning_text","text":"Both read; answer."}]},
	{"type":"message","role":"assistant","content":[{"type":"output_text","text":"15"}]},
	{"type":"reasoning","summary":[],"encrypted_content":"gAAAA-opaque"},
	{"type":"message","role":"assistant","content":[{"type":"output_text","text":"more"}]},
	{"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]},
	{"type":"message","role":"user","content":"next"},
	{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]}
]}`

func TestResponsesHistoryReasoningPassedBackToDeepSeek(t *testing.T) {
	msgs := buildMessages(t, "deepseek-v4-pro", "openai-response", responsesReasoningHistory)
	// user, assistant(tool_calls c1+c2), tool, tool, assistant "15",
	// assistant "more", user, assistant "final"
	if len(msgs) != 8 {
		t.Fatalf("message count = %d: %v", len(msgs), msgs)
	}
	calls := msgs[1]
	if calls["role"] != "assistant" || len(calls["tool_calls"].([]any)) != 2 ||
		calls["reasoning_content"] != "Need the file." {
		t.Fatalf("tool-call turn must carry its summary reasoning once: %v", calls)
	}
	if msgs[4]["content"] != "15" || msgs[4]["reasoning_content"] != "Both read; answer." {
		t.Fatalf("raw reasoning_text must win over the summary: %v", msgs[4])
	}
	if _, ok := msgs[5]["reasoning_content"]; ok {
		t.Fatalf("encrypted-only reasoning carries no plaintext: %v", msgs[5])
	}
	// The orphan reasoning before the user turn is dropped there, never
	// leaking onto the next assistant answer.
	for _, i := range []int{0, 2, 3, 6, 7} {
		if _, ok := msgs[i]["reasoning_content"]; ok {
			t.Fatalf("message %d must not carry reasoning_content: %v", i, msgs[i])
		}
	}
}

// Reasoning followed by a tool output (no assistant output in between)
// belongs to no replayed answer and is dropped at the tool message.
func TestResponsesHistoryReasoningDroppedAtToolOutput(t *testing.T) {
	msgs := buildMessages(t, "deepseek-v4-pro", "openai-response", `{"input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"stray"}]},
		{"type":"function_call_output","call_id":"c1","output":"x"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
	]}`)
	if len(msgs) != 3 {
		t.Fatalf("message count = %d: %v", len(msgs), msgs)
	}
	for i, m := range msgs {
		if _, ok := m["reasoning_content"]; ok {
			t.Fatalf("message %d must not carry the stray reasoning: %v", i, m)
		}
	}
}

func TestResponsesHistoryReasoningDroppedForOtherFamilies(t *testing.T) {
	for _, m := range buildMessages(t, "glm-5.3", "openai-response", responsesReasoningHistory) {
		if _, ok := m["reasoning_content"]; ok {
			t.Fatalf("reasoning_content must stay omitted: %v", m)
		}
	}
}
