package shared

import (
	"encoding/json"
	"testing"
)

func TestClaudeOutputEffort(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{``, ""},
		{`null`, ""},
		{`{}`, ""},
		{`{"effort":null}`, ""},
		{`{"effort":"max"}`, "max"},
		{`{"effort":" XHigh "}`, "xhigh"},
		{`{"effort":3}`, ""},
		{`{"effort":{"level":"max"}}`, ""},
		{`"max"`, ""},
		{`[1,2]`, ""},
	}
	for _, c := range cases {
		if got := ClaudeOutputEffort(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("ClaudeOutputEffort(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestDecodeClaudeMessagesOutputEffort(t *testing.T) {
	rec, eErr := DecodeClaudeMessages(json.RawMessage(
		`{"max_tokens":8,"thinking":{"type":"adaptive"},"output_config":{"effort":"High"},"messages":[]}`))
	if eErr != nil {
		t.Fatalf("decode: %v", eErr)
	}
	if rec.OutputEffort != "high" || rec.Thinking == nil || rec.Thinking.Type != "adaptive" {
		t.Fatalf("record = %+v, want adaptive thinking with effort high", rec)
	}
	// A malformed hint must never fail an otherwise valid request.
	if _, eErr := DecodeClaudeMessages(json.RawMessage(
		`{"max_tokens":8,"output_config":{"effort":7},"messages":[]}`)); eErr != nil {
		t.Fatalf("non-string effort must be tolerated: %v", eErr)
	}
}

func TestDecodeClaudeMessagesThinkingText(t *testing.T) {
	rec, eErr := DecodeClaudeMessages(json.RawMessage(`{"max_tokens":8,"messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"plan","signature":""},
		{"type":"thinking","thinking":{"not":"a string"}},
		{"type":"redacted_thinking","data":"enc"},
		{"type":"text","text":"answer","thinking":"ignored on text blocks"}
	]}]}`))
	if eErr != nil {
		t.Fatalf("decode: %v", eErr)
	}
	blocks := rec.Messages[0].Blocks
	if blocks[0].Kind != "thinking" || blocks[0].Text != "plan" {
		t.Fatalf("thinking text not carried: %+v", blocks[0])
	}
	if blocks[1].Kind != "thinking" || blocks[1].Text != "" {
		t.Fatalf("non-string thinking must decode leniently to empty: %+v", blocks[1])
	}
	if blocks[2].Text != "" || blocks[3].Text != "answer" {
		t.Fatalf("other blocks changed: %+v %+v", blocks[2], blocks[3])
	}
}
