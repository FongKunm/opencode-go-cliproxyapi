package sanitize

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// sameSlice reports whether out is body itself (not a copy).
func sameSlice(body, out []byte) bool {
	return len(body) == len(out) && (len(body) == 0 || &body[0] == &out[0])
}

// A typical Claude Code turn has nothing to rewrite and must pass through
// untouched, without re-encoding.
func TestClaudeMessagesNoopReturnsInput(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":true,
	  "system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
	  "tools":[{"name":"Bash","description":"run <cmd> & more","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},
	           {"type":"custom","name":"x","input_schema":{"type":"object"}},{"type":"bash_20250124","name":"bash"}],
	  "tool_choice":{"type":"auto"},
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"redacted_thinking","data":"r"},{"type":"text","text":"ok"},{"type":"tool_use","id":"a","name":"Bash","input":{"command":"ls"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"f"},{"type":"tool_reference","tool_name":"Bash"},{"type":"image","source":{"type":"url","url":"https://x/i.png"}}]}]},
	    {"role":"user","content":"plain"}]}`)
	if out := ClaudeMessages(body); !sameSlice(body, out) {
		t.Fatalf("rewrote a clean request:\n%s", out)
	}
}

func TestClaudeMessagesLeavesMalformedInputAlone(t *testing.T) {
	for _, body := range []string{
		``, `null`, `[]`, `"x"`, `{`,
		`{"messages":"nope"}`,
		`{"messages":[{"role":"user","content":[42,"str"]}]}`,
		`{"messages":[{"role":"robot","content":[{"type":"video"}]}]}`,
		`{"system":[7],"tools":"x"}`,
		`{"messages":[{"role":"user","content":[{"type":"image"},{"type":"image","source":{"type":"base64"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":{"type":"text"}}]}]}`,
	} {
		b := []byte(body)
		if out := ClaudeMessages(b); !sameSlice(b, out) {
			t.Fatalf("%s: rewritten to %s", body, out)
		}
	}
}

func TestClaudeMessagesUntypedBlocks(t *testing.T) {
	out := ClaudeMessages([]byte(`{"messages":[{"role":"user","content":[{"text":"loose text"},{"foo":1}]}]}`))
	var req struct {
		Messages []struct {
			Content []map[string]string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	c := req.Messages[0].Content
	if len(c) != 2 || c[0]["text"] != "loose text" || c[1]["text"] != "[untyped content omitted]" || c[1]["type"] != "text" {
		t.Fatalf("content = %v", c)
	}
}

func TestClaudeMessagesKeepsOtherFieldsAndHTML(t *testing.T) {
	out := ClaudeMessages([]byte(`{"metadata":{"user_id":"u<1>"},"messages":[{"role":"user","id":"keep-me","content":[{"type":"widget"},{"type":"text","text":"a & <b>"}]}]}`))
	s := string(out)
	for _, want := range []string{`"metadata":{"user_id":"u<1>"}`, `"id":"keep-me"`, `"text":"a & <b>"`, `"text":"[widget content omitted]"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestResponsesNoopReturnsInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5-codex","instructions":"You are Codex.","stream":true,"tool_choice":"auto",
	  "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch"},
	           {"type":"namespace","name":"mcp","tools":[{"type":"function","name":"echo"}]}],
	  "input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules <x> & y"}]},
	    {"role":"user","content":"hi"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]},
	    {"type":"reasoning","summary":[],"encrypted_content":"gAAA"},
	    {"type":"function_call","call_id":"c","name":"shell","arguments":"{}"},
	    {"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"ok"}]},
	    {"type":"custom_tool_call","call_id":"d","name":"apply_patch","input":"x"},
	    {"type":"custom_tool_call_output","call_id":"d","output":"done"},
	    {"type":"additional_tools","tools":[{"type":"function","name":"more"}]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"fine"}]}]}`)
	if out := Responses(body); !sameSlice(body, out) {
		t.Fatalf("rewrote a clean request:\n%s", out)
	}
}

func TestResponsesLeavesMalformedInputAlone(t *testing.T) {
	for _, body := range []string{
		``, `null`, `[]`, `{`,
		`{"input":"just text"}`,
		`{"input":[42,"x"]}`,
		`{"input":[{"type":"message","role":"robot","content":[{"type":"input_video"}]}]}`,
		`{"input":[{"type":"message","role":"user","content":[{"type":"input_image"}]}]}`,
		`{"input":[{"type":"function_call_output","call_id":"c","output":[7]}]}`,
		`{"tools":"x","tool_choice":{"type":"function","name":"f"}}`,
	} {
		b := []byte(body)
		if out := Responses(b); !sameSlice(b, out) {
			t.Fatalf("%s: rewritten to %s", body, out)
		}
	}
}

func TestDocumentTextTruncated(t *testing.T) {
	long := strings.Repeat("ab", maxDocChars) // twice the cap
	d := doc{title: "big.txt", text: long, hasText: true}
	got := d.render()
	if !strings.HasPrefix(got, "[Document: big.txt]\n") ||
		!strings.HasSuffix(got, "[Document truncated: only the first 200000 characters are included.]") {
		t.Fatalf("render = %.80q ... %.120q", got, got[len(got)-120:])
	}
	if n := strings.Count(got, "ab"); n != maxDocChars/2 {
		t.Fatalf("kept %d pairs, want %d", n, maxDocChars/2)
	}
}

func TestBase64TextLargeFileReadsOnlyThePrefix(t *testing.T) {
	text := strings.Repeat("中文", maxDocChars) // 1.2 MB of UTF-8, beyond the read limit
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	got, truncated, ok := base64Text(b64)
	if !ok || !truncated || !utf8.ValidString(got) || !strings.HasPrefix(text, got) || len(got) > maxDocChars*4 {
		t.Fatalf("ok=%v truncated=%v len=%d valid=%v", ok, truncated, len(got), utf8.ValidString(got))
	}
	d := doc{mediaType: "text/plain; charset=utf-8", b64: b64}
	if r := d.render(); utf8.RuneCountInString(r) > maxDocChars+200 || !strings.Contains(r, "[Document truncated") {
		t.Fatalf("render length %d", utf8.RuneCountInString(r))
	}
}

func TestDecodeBase64Variants(t *testing.T) {
	data := []byte{0xfb, 0xff, 0xfe, 'h', 'i'}
	for _, s := range []string{
		base64.StdEncoding.EncodeToString(data),
		base64.RawStdEncoding.EncodeToString(data),
		base64.URLEncoding.EncodeToString(data),
		base64.RawURLEncoding.EncodeToString(data),
		"+/" + "/+aGk=\r\n",
	} {
		if got, ok := decodeBase64(s, 100); !ok || string(got) != string(data) {
			t.Fatalf("decodeBase64(%q) = %v, %v", s, got, ok)
		}
	}
	if _, ok := decodeBase64(base64.StdEncoding.EncodeToString(make([]byte, 200)), 100); ok {
		t.Fatal("payload over the limit decoded")
	}
}

func TestResponsesFileSources(t *testing.T) {
	pdfish := base64.StdEncoding.EncodeToString([]byte("not really a pdf"))
	cases := []struct {
		part string
		want string
	}{
		{`{"type":"input_file","filename":"a.txt","file_data":"data:text/plain;base64,` + base64.StdEncoding.EncodeToString([]byte("hello")) + `"}`, "[Document: a.txt]\nhello"},
		{`{"type":"input_file","filename":"notes.md","file_data":"` + base64.StdEncoding.EncodeToString([]byte("# Title")) + `"}`, "[Document: notes.md]\n# Title"},
		{`{"type":"input_file","filename":"r.pdf","file_data":"data:application/pdf;base64,` + pdfish + `"}`, "[Document: r.pdf (PDF)]\n(The PDF text could not be extracted: the PDF could not be parsed.)"},
		{`{"type":"input_file","file_url":"https://x/y.pdf"}`, "[Document]\n(The document is at https://x/y.pdf and was not fetched.)"},
		{`{"type":"input_file","file_id":"file-123","filename":"z.docx"}`, "[Document: z.docx]\n(The document is an uploaded file (file-123) that is not available here.)"},
	}
	for _, c := range cases {
		m, _ := object(json.RawMessage(c.part))
		if got := responsesFile(m).render(); got != c.want {
			t.Fatalf("%s:\n got %q\nwant %q", c.part, got, c.want)
		}
	}
}
