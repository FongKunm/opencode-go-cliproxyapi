package shared

import (
	"encoding/json"
	"testing"
)

func TestRespReasoningItemJSONShape(t *testing.T) {
	b, _ := json.Marshal(NewRespReasoningItem("rs_1", "think"))
	if got := string(b); got != `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}` {
		t.Fatalf("reasoning item = %s", got)
	}
	// Codex decodes summary as a required array: never null, even empty.
	b, _ = json.Marshal(NewRespReasoningItem("rs_2", ""))
	if got := string(b); got != `{"type":"reasoning","id":"rs_2","summary":[]}` {
		t.Fatalf("empty reasoning item = %s", got)
	}
}

func TestResponsesReasoningEvents(t *testing.T) {
	em := ResponsesEventEmitter{ID: "r", Model: "m"}
	cases := []struct {
		frame []byte
		want  string
	}{
		{em.ReasoningItemAdded(0, "rs_r"),
			"event: response.output_item.added\ndata: " +
				`{"item":{"id":"rs_r","summary":[],"type":"reasoning"},"output_index":0,"type":"response.output_item.added"}` + "\n\n"},
		{em.ReasoningSummaryPartAdded("rs_r", 0),
			"event: response.reasoning_summary_part.added\ndata: " +
				`{"item_id":"rs_r","output_index":0,"part":{"text":"","type":"summary_text"},"summary_index":0,"type":"response.reasoning_summary_part.added"}` + "\n\n"},
		{em.ReasoningSummaryTextDelta("rs_r", 0, "a"),
			"event: response.reasoning_summary_text.delta\ndata: " +
				`{"delta":"a","item_id":"rs_r","output_index":0,"summary_index":0,"type":"response.reasoning_summary_text.delta"}` + "\n\n"},
		{em.ReasoningSummaryTextDone("rs_r", 0, "ab"),
			"event: response.reasoning_summary_text.done\ndata: " +
				`{"item_id":"rs_r","output_index":0,"summary_index":0,"text":"ab","type":"response.reasoning_summary_text.done"}` + "\n\n"},
		{em.ReasoningSummaryPartDone("rs_r", 0, "ab"),
			"event: response.reasoning_summary_part.done\ndata: " +
				`{"item_id":"rs_r","output_index":0,"part":{"text":"ab","type":"summary_text"},"summary_index":0,"type":"response.reasoning_summary_part.done"}` + "\n\n"},
	}
	for _, c := range cases {
		if string(c.frame) != c.want {
			t.Errorf("frame\n got: %q\nwant: %q", c.frame, c.want)
		}
	}
}
