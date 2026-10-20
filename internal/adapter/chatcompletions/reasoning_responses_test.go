package chatcompletions

import (
	"strings"
	"testing"
)

// ---- non-stream: reasoning_content → leading reasoning item ----

func outputItems(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["output"].([]any)
	if !ok {
		t.Fatalf("output not an array: %v", m)
	}
	out := make([]map[string]any, len(raw))
	for i, it := range raw {
		out[i] = it.(map[string]any)
	}
	return out
}

func assertReasoningItem(t *testing.T, item map[string]any, id, text string) {
	t.Helper()
	if item["type"] != "reasoning" || item["id"] != id {
		t.Fatalf("reasoning item identity = %v, want reasoning %q", item, id)
	}
	summary, ok := item["summary"].([]any)
	if !ok || len(summary) != 1 {
		t.Fatalf("reasoning summary must be a one-part array: %v", item)
	}
	part := summary[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != text {
		t.Fatalf("summary part = %v, want summary_text %q", part, text)
	}
	if _, ok := item["encrypted_content"]; ok {
		t.Fatalf("no encrypted_content may be fabricated: %v", item)
	}
}

func itemTypes(items []map[string]any) string {
	var types []string
	for _, it := range items {
		types = append(types, it["type"].(string))
	}
	return strings.Join(types, ",")
}

func TestConvertNonStreamResponsesReasoning(t *testing.T) {
	t.Run("reasoning precedes message with reasoning tokens", func(t *testing.T) {
		m := mustConvert(t, "openai-response", `{"id":"r","model":"deepseek-v4-pro","choices":[{"finish_reason":"stop",`+
			`"message":{"content":"15","reasoning_content":"We need to add."}}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}`)
		items := outputItems(t, m)
		if itemTypes(items) != "reasoning,message" {
			t.Fatalf("output order = %s", itemTypes(items))
		}
		assertReasoningItem(t, items[0], "rs_r", "We need to add.")
		if items[1]["id"] != "r" {
			t.Fatalf("message identity must stay the response id: %v", items[1])
		}
		u := m["usage"].(map[string]any)
		if u["output_tokens"] != float64(9) ||
			u["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(6) {
			t.Fatalf("usage = %v, want reasoning_tokens carried", u)
		}
	})
	t.Run("reasoning precedes message and function calls", func(t *testing.T) {
		m := mustConvert(t, "openai-response", `{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",`+
			`"message":{"content":"Checking.","reasoning_content":"Need both.","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},`+
			`{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]}}]}`)
		items := outputItems(t, m)
		if itemTypes(items) != "reasoning,message,function_call,function_call" {
			t.Fatalf("output order = %s", itemTypes(items))
		}
		assertReasoningItem(t, items[0], "rs_r", "Need both.")
	})
	t.Run("reasoning with function calls only", func(t *testing.T) {
		m := mustConvert(t, "openai-response", `{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",`+
			`"message":{"content":null,"reasoning_content":"Call f.","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
		if got := itemTypes(outputItems(t, m)); got != "reasoning,function_call" {
			t.Fatalf("output order = %s", got)
		}
	})
	t.Run("reasoning only", func(t *testing.T) {
		m := mustConvert(t, "openai-response", `{"id":"r","model":"m","choices":[{"finish_reason":"length",`+
			`"message":{"content":"","reasoning_content":"Still going"}}]}`)
		items := outputItems(t, m)
		if itemTypes(items) != "reasoning" || m["status"] != "incomplete" {
			t.Fatalf("output = %s status = %v", itemTypes(items), m["status"])
		}
		assertReasoningItem(t, items[0], "rs_r", "Still going")
	})
	t.Run("no reasoning leaves output unchanged", func(t *testing.T) {
		for _, msg := range []string{
			`{"content":"ok"}`, `{"content":"ok","reasoning_content":null}`, `{"content":"ok","reasoning_content":""}`,
		} {
			m := mustConvert(t, "openai-response", `{"id":"r","model":"m","choices":[{"finish_reason":"stop","message":`+msg+`}]}`)
			if got := itemTypes(outputItems(t, m)); got != "message" {
				t.Fatalf("%s: output = %s, want the lone message", msg, got)
			}
		}
	})
}

// ---- stream: reasoning deltas → reasoning item lifecycle ----

func eventNames(evs []sseEvt) []string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = e.Name
	}
	return names
}

// assertAnnouncementsMatchCompleted pins that every output_item.added and
// output_item.done index addresses the same item the terminal
// response.completed output array holds at that position.
func assertAnnouncementsMatchCompleted(t *testing.T, evs []sseEvt) []any {
	t.Helper()
	var output []any
	for _, e := range evs {
		if e.Name == "response.completed" {
			output = e.Data["response"].(map[string]any)["output"].([]any)
		}
	}
	if output == nil {
		t.Fatalf("missing response.completed: %v", eventNames(evs))
	}
	added := 0
	for _, e := range evs {
		if e.Name != "response.output_item.added" && e.Name != "response.output_item.done" {
			continue
		}
		idx := int(e.Data["output_index"].(float64))
		if idx >= len(output) {
			t.Fatalf("%s index %d beyond completed output %v", e.Name, idx, output)
		}
		want := output[idx].(map[string]any)
		got := e.Data["item"].(map[string]any)
		if got["type"] != want["type"] {
			t.Fatalf("%s index %d type %v, completed holds %v", e.Name, idx, got["type"], want["type"])
		}
		if id, ok := want["id"]; ok && got["type"] != "function_call" && got["id"] != id {
			t.Fatalf("%s index %d id %v, completed holds %v", e.Name, idx, got["id"], id)
		}
		if e.Name == "response.output_item.added" {
			added++
		}
	}
	if added != len(output) {
		t.Fatalf("announced %d items, completed carries %d", added, len(output))
	}
	return output
}

func TestStreamResponsesReasoningThenText(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	evs := feedAll(t, sc,
		`data: {"id":"r1","model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""}}]}`,
		`data: {"choices":[{"delta":{"content":null,"reasoning_content":"We need"}}]}`,
		`data: {"choices":[{"delta":{"content":null,"reasoning_content":" to add."}}]}`,
		`data: {"choices":[{"delta":{"content":"15","reasoning_content":null}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}`,
		`data: [DONE]`)
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(eventNames(evs), ",") != strings.Join(want, ",") {
		t.Fatalf("event order\n got: %v\nwant: %v", eventNames(evs), want)
	}
	added := evs[1].Data
	if added["output_index"] != float64(0) {
		t.Fatalf("reasoning must take output index 0: %v", added)
	}
	if it := added["item"].(map[string]any); it["type"] != "reasoning" || it["id"] != "rs_r1" ||
		len(it["summary"].([]any)) != 0 {
		t.Fatalf("reasoning announcement = %v", it)
	}
	for _, e := range evs[2:7] {
		if e.Data["item_id"] != "rs_r1" || e.Data["output_index"] != float64(0) || e.Data["summary_index"] != float64(0) {
			t.Fatalf("%s addresses the wrong item: %v", e.Name, e.Data)
		}
	}
	if evs[3].Data["delta"] != "We need" || evs[4].Data["delta"] != " to add." {
		t.Fatalf("summary deltas wrong: %v %v", evs[3].Data, evs[4].Data)
	}
	if evs[5].Data["text"] != "We need to add." ||
		evs[6].Data["part"].(map[string]any)["text"] != "We need to add." {
		t.Fatalf("summary done events wrong: %v %v", evs[5].Data, evs[6].Data)
	}
	assertReasoningItem(t, evs[7].Data["item"].(map[string]any), "rs_r1", "We need to add.")
	if evs[8].Data["output_index"] != float64(1) || evs[9].Data["output_index"] != float64(1) {
		t.Fatalf("message must follow at index 1: %v %v", evs[8].Data, evs[9].Data)
	}
	output := assertAnnouncementsMatchCompleted(t, evs)
	assertReasoningItem(t, output[0].(map[string]any), "rs_r1", "We need to add.")
	u := evs[len(evs)-1].Data["response"].(map[string]any)["usage"].(map[string]any)
	if u["output_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(6) {
		t.Fatalf("completed usage must carry reasoning_tokens: %v", u)
	}
}

func TestStreamResponsesReasoningThenTools(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning_content":"Read both."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	// The reasoning item completes before the first function_call is announced.
	var seq []string
	for _, e := range evs {
		if e.Name == "response.output_item.added" || e.Name == "response.output_item.done" {
			seq = append(seq, e.Name+":"+e.Data["item"].(map[string]any)["type"].(string))
		}
	}
	wantSeq := []string{
		"response.output_item.added:reasoning", "response.output_item.done:reasoning",
		"response.output_item.added:function_call", "response.output_item.added:function_call",
		"response.output_item.done:function_call", "response.output_item.done:function_call",
	}
	if strings.Join(seq, ",") != strings.Join(wantSeq, ",") {
		t.Fatalf("item lifecycle\n got: %v\nwant: %v", seq, wantSeq)
	}
	output := assertAnnouncementsMatchCompleted(t, evs)
	if len(output) != 3 {
		t.Fatalf("completed output = %v", output)
	}
}

// Reasoning that resumes after the message and before a tool call becomes
// a second reasoning item with its own id and index; every announced index
// still lines up with the completed output.
func TestStreamResponsesReasoningResumes(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning_content":"a"}}]}`,
		`data: {"choices":[{"delta":{"content":"x"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"b"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	output := assertAnnouncementsMatchCompleted(t, evs)
	var types []string
	for _, it := range output {
		types = append(types, it.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "reasoning,message,reasoning,function_call" {
		t.Fatalf("completed output order = %v", types)
	}
	assertReasoningItem(t, output[0].(map[string]any), "rs_r", "a")
	assertReasoningItem(t, output[2].(map[string]any), "rs_r_1", "b")
}

func TestStreamResponsesReasoningOnlyAndFlush(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	evs = append(evs, parseEvents(t, sc.Flush())...)
	tail := eventNames(evs)[len(evs)-4:]
	want := []string{
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done",
		"response.output_item.done", "response.completed",
	}
	if strings.Join(tail, ",") != strings.Join(want, ",") {
		t.Fatalf("terminal must complete the open reasoning item first: %v", eventNames(evs))
	}
	output := assertAnnouncementsMatchCompleted(t, evs)
	assertReasoningItem(t, output[0].(map[string]any), "rs_r", "thinking...")
	if evs[len(evs)-1].Data["response"].(map[string]any)["status"] != "incomplete" {
		t.Fatalf("length must stay incomplete: %v", evs[len(evs)-1].Data)
	}
}

func TestStreamResponsesNoReasoningUnchanged(t *testing.T) {
	plain := []string{
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	nulls := []string{
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant","reasoning_content":null}}]}`,
		`data: {"choices":[{"delta":{"content":"hi","reasoning_content":""}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}],"reasoning":null}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	a := feedAll(t, NewStreamConverter("openai-response"), plain...)
	b := feedAll(t, NewStreamConverter("openai-response"), nulls...)
	if strings.Join(eventNames(a), ",") != strings.Join(eventNames(b), ",") {
		t.Fatalf("empty reasoning fields changed the stream:\n%v\n%v", eventNames(a), eventNames(b))
	}
	for _, e := range b {
		if strings.Contains(e.Name, "reasoning") {
			t.Fatalf("unexpected reasoning event: %v", e)
		}
	}
	assertAnnouncementsMatchCompleted(t, b)
}
