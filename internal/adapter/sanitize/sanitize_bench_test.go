package sanitize

import (
	"fmt"
	"strings"
	"testing"
)

// claudeCodeBody builds a long Claude Code style conversation (about 2 MB)
// with thinking, tool calls and tool results; extra is appended to the
// last user turn.
func claudeCodeBody(extra string) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":true,` +
		`"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],"messages":[`)
	chunk := strings.Repeat("line of tool output with some code fmt.Println(x) ", 40)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, `{"role":"user","content":[{"type":"text","text":"step %d please"}]},`, i)
		fmt.Fprintf(&b, `{"role":"assistant","content":[{"type":"thinking","thinking":"%s","signature":"sig"},`+
			`{"type":"text","text":"running"},{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"ls"}}]},`, chunk[:400], i)
		fmt.Fprintf(&b, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":[{"type":"text","text":"%s"}]}]},`, i, chunk)
	}
	fmt.Fprintf(&b, `{"role":"user","content":[{"type":"text","text":"done?"}%s]}]}`, extra)
	return []byte(b.String())
}

func codexBody(extra string) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5-codex","instructions":"You are Codex.","tools":[{"type":"function","name":"shell"}],"input":[`)
	chunk := strings.Repeat("line of shell output with some code fmt.Println(x) ", 40)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, `{"type":"message","role":"user","content":[{"type":"input_text","text":"step %d"}]},`, i)
		fmt.Fprintf(&b, `{"type":"reasoning","summary":[],"encrypted_content":"%s"},`, chunk[:400])
		fmt.Fprintf(&b, `{"type":"function_call","call_id":"c%d","name":"shell","arguments":"{}"},`, i)
		fmt.Fprintf(&b, `{"type":"function_call_output","call_id":"c%d","output":"%s"},`, i, chunk)
	}
	fmt.Fprintf(&b, `{"type":"message","role":"user","content":[{"type":"input_text","text":"done?"}%s]}]}`, extra)
	return []byte(b.String())
}

func BenchmarkClaudeMessagesNoop(b *testing.B) {
	body := claudeCodeBody("")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ClaudeMessages(body)
	}
}

func BenchmarkClaudeMessagesRewrite(b *testing.B) {
	body := claudeCodeBody(`,{"type":"document","source":{"type":"text","data":"notes"}}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ClaudeMessages(body)
	}
}

func BenchmarkResponsesNoop(b *testing.B) {
	body := codexBody("")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Responses(body)
	}
}

func BenchmarkResponsesRewrite(b *testing.B) {
	body := codexBody(`,{"type":"input_file","file_id":"file-1"}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Responses(body)
	}
}
