package chatcompletions

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Requests whose history holds content the Chat Completions route cannot
// represent must translate (sanitize rewrites it) instead of failing every
// later turn with a 400.

// minimalPDF builds a one-page PDF showing the given lines of text.
func minimalPDF(lines ...string) []byte {
	var content strings.Builder
	content.WriteString("BT /F1 12 Tf 14 TL 72 720 Td ")
	for _, l := range lines {
		fmt.Fprintf(&content, "(%s) Tj T* ", l)
	}
	content.WriteString("ET")
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write([]byte(content.String()))
	w.Close()
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", z.Len(), z.Bytes()),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs)+1)
	for i, o := range objs {
		offs[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offs[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func pdfB64(lines ...string) string { return base64.StdEncoding.EncodeToString(minimalPDF(lines...)) }

// ccMsgs translates a body and returns its Chat Completions messages.
func ccMsgs(t *testing.T, format, body string) (map[string]any, []map[string]any) {
	t.Helper()
	m := mustBuild(t, format, body, nil)
	raw, _ := m["messages"].([]any)
	msgs := make([]map[string]any, len(raw))
	for i, r := range raw {
		msgs[i] = r.(map[string]any)
	}
	return m, msgs
}

// textOf flattens a message's content (string or parts) to its text.
func textOf(msg map[string]any) string {
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				b.WriteString(pm["text"].(string))
			}
		}
		return b.String()
	}
	return ""
}

func imageURLs(msg map[string]any) []string {
	var urls []string
	parts, _ := msg["content"].([]any)
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
			urls = append(urls, pm["image_url"].(map[string]any)["url"].(string))
		}
	}
	return urls
}

func wantAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
}

func wantNone(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(got, w) {
			t.Fatalf("unexpected %q in:\n%s", w, got)
		}
	}
}

func userMsg(blocks string) string {
	return `{"model":"x","max_tokens":100,"messages":[{"role":"user","content":[` + blocks + `]}]}`
}

func assistantMsg(blocks string) string {
	return `{"model":"x","max_tokens":100,"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[` + blocks + `]},{"role":"user","content":"next"}]}`
}

func toolResultMsg(content string) string {
	return `{"model":"x","max_tokens":100,"messages":[` +
		`{"role":"user","content":"read it"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/tmp/a.pdf"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` + content + `]}]}]}`
}

func TestSanitizeClaudeUserBlocks(t *testing.T) {
	cases := []struct {
		name, block string
		want        []string
	}{
		{"document text source",
			`{"type":"document","title":"Notes","context":"from the wiki","source":{"type":"text","media_type":"text/plain","data":"hello doc"}}`,
			[]string{"[Document: Notes]", "Context: from the wiki", "hello doc"}},
		{"document PDF",
			`{"type":"document","title":"Report","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64("Revenue grew 12 percent", "Costs fell") + `"}}`,
			[]string{"[Document: Report (PDF, 1 page)]", "Revenue grew 12 percent\nCosts fell"}},
		{"document corrupt PDF",
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 garbage")) + `"}}`,
			[]string{"[Document (PDF)]", "could not be extracted: the PDF could not be parsed"}},
		{"document PDF bad base64",
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"@@not base64@@"}}`,
			[]string{"[Document (PDF)]", "(The PDF data could not be decoded.)"}},
		{"document base64 text file",
			`{"type":"document","title":"data.csv","source":{"type":"base64","media_type":"text/csv","data":"` + base64.StdEncoding.EncodeToString([]byte("a,b\n1,2")) + `"}}`,
			[]string{"[Document: data.csv]", "a,b\n1,2"}},
		{"document unsupported media",
			`{"type":"document","source":{"type":"base64","media_type":"application/zip","data":"UEsDBA=="}}`,
			[]string{"(application/zip content is not supported here.)"}},
		{"document url source",
			`{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}`,
			[]string{"(The document is at https://example.com/a.pdf and was not fetched.)"}},
		{"document file source",
			`{"type":"document","source":{"type":"file","file_id":"file_011"}}`,
			[]string{"uploaded file (file_011) that is not available here"}},
		{"document content source",
			`{"type":"document","source":{"type":"content","content":[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]}}`,
			[]string{"part one\n\npart two"}},
		{"search_result",
			`{"type":"search_result","source":"https://kb.example/x","title":"KB article","content":[{"type":"text","text":"Answer text"}],"citations":{"enabled":true}}`,
			[]string{"[Search result: KB article]", "Source: https://kb.example/x", "Answer text"}},
		{"container_upload",
			`{"type":"container_upload","file_id":"file_abc"}`,
			[]string{"[File uploaded to the code execution container: file_abc]"}},
		{"image with file source",
			`{"type":"image","source":{"type":"file","file_id":"file_img"}}`,
			[]string{"[image omitted: uploaded file file_img is not available here]"}},
		{"image with unknown source",
			`{"type":"image","source":{"type":"s3","bucket":"b"}}`,
			[]string{"[image omitted: unsupported source s3]"}},
		{"unknown future block",
			`{"type":"video","source":{"type":"base64","data":"AAAA"}}`,
			[]string{"[video content omitted]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, msgs := ccMsgs(t, "claude", userMsg(`{"type":"text","text":"look:"},`+c.block))
			if len(msgs) != 1 || msgs[0]["role"] != "user" {
				t.Fatalf("messages = %v", msgs)
			}
			got := textOf(msgs[0])
			wantAll(t, got, append([]string{"look:"}, c.want...)...)
		})
	}
}

func TestSanitizeClaudeDocumentContentImagesKept(t *testing.T) {
	_, msgs := ccMsgs(t, "claude", userMsg(`{"type":"document","source":{"type":"content","content":[`+
		`{"type":"text","text":"caption"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}}`))
	wantAll(t, textOf(msgs[0]), "caption")
	if urls := imageURLs(msgs[0]); len(urls) != 1 || urls[0] != "data:image/png;base64,QUJD" {
		t.Fatalf("document image not kept: %v", msgs[0])
	}
}

func TestSanitizeClaudeUserThinkingDropped(t *testing.T) {
	_, msgs := ccMsgs(t, "claude", userMsg(`{"type":"thinking","thinking":"x","signature":"s"},{"type":"redacted_thinking","data":"y"},{"type":"text","text":"hi"}`))
	if len(msgs) != 1 || msgs[0]["content"] != "hi" {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestSanitizeClaudeAssistantBlocks(t *testing.T) {
	cases := []struct {
		name, blocks string
		want         []string
	}{
		{"server tool use and web search results",
			`{"type":"text","text":"Searching."},` +
				`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go 1.26 release"}},` +
				`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[` +
				`{"type":"web_search_result","url":"https://go.dev/doc/go1.26","title":"Go 1.26 Release Notes","encrypted_content":"EqgfCioIARgB","page_age":"February 2026"},` +
				`{"type":"web_search_result","url":"https://go.dev/blog","title":"The Go Blog","encrypted_content":"EqgfCioIARgC"}]},` +
				`{"type":"text","text":"Go 1.26 shipped in February.","citations":[{"type":"web_search_result_location","url":"https://go.dev/doc/go1.26","title":"Go 1.26 Release Notes","encrypted_index":"Eo8BCioIAhgB","cited_text":"Go 1.26"}]}`,
			[]string{"Searching.\n\n[Server tool call: web_search {\"query\":\"go 1.26 release\"}]\n\n[Web search results]\n1. Go 1.26 Release Notes\n   https://go.dev/doc/go1.26\n   Page age: February 2026\n2. The Go Blog\n   https://go.dev/blog\n\nGo 1.26 shipped in February."}},
		{"web search error",
			`{"type":"web_search_tool_result","tool_use_id":"s","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}`,
			[]string{"[Web search failed: max_uses_exceeded]"}},
		{"web search no results",
			`{"type":"web_search_tool_result","tool_use_id":"s","content":[]}`,
			[]string{"[Web search returned no results]"}},
		{"web fetch result",
			`{"type":"web_fetch_tool_result","tool_use_id":"s","content":{"type":"web_fetch_result","url":"https://example.com/post","retrieved_at":"2026-10-01T00:00:00Z",` +
				`"content":{"type":"document","title":"A post","source":{"type":"text","media_type":"text/plain","data":"Body of the post"}}}}`,
			[]string{"[Fetched https://example.com/post at 2026-10-01T00:00:00Z]", "[Document: A post]", "Body of the post"}},
		{"web fetch PDF result",
			`{"type":"web_fetch_tool_result","tool_use_id":"s","content":{"type":"web_fetch_result","url":"https://example.com/a.pdf",` +
				`"content":{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64("fetched pdf text") + `"}}}}`,
			[]string{"[Fetched https://example.com/a.pdf]", "fetched pdf text"}},
		{"web fetch error",
			`{"type":"web_fetch_tool_result","tool_use_id":"s","content":{"type":"web_fetch_tool_result_error","error_code":"url_not_accessible"}}`,
			[]string{"[Web fetch failed: url_not_accessible]"}},
		{"code execution result",
			`{"type":"server_tool_use","id":"c1","name":"code_execution","input":{"code":"print(1)"}},` +
				`{"type":"code_execution_tool_result","tool_use_id":"c1","content":{"type":"code_execution_result","stdout":"1\n","stderr":"warn","return_code":0,"content":[{"type":"code_execution_output","file_id":"file_out"}]}}`,
			[]string{"[Server tool call: code_execution {\"code\":\"print(1)\"}]", "[Code execution result, exit code 0]", "stdout:\n1\n", "stderr:\nwarn", "Output files: file_out"}},
		{"bash code execution result",
			`{"type":"bash_code_execution_tool_result","tool_use_id":"c2","content":{"type":"bash_code_execution_result","stdout":"ok","stderr":"","return_code":2,"content":[]}}`,
			[]string{"[Code execution result, exit code 2]", "stdout:\nok"}},
		{"code execution error",
			`{"type":"code_execution_tool_result","tool_use_id":"c3","content":{"type":"code_execution_tool_result_error","error_code":"unavailable"}}`,
			[]string{"[Code execution failed: unavailable]"}},
		{"text editor view",
			`{"type":"text_editor_code_execution_tool_result","tool_use_id":"e","content":{"type":"text_editor_code_execution_view_result","file_type":"text","content":"line 1\nline 2","num_lines":2}}`,
			[]string{"[Text editor: viewed file]\nline 1\nline 2"}},
		{"text editor create",
			`{"type":"text_editor_code_execution_tool_result","tool_use_id":"e","content":{"type":"text_editor_code_execution_create_result","is_file_update":true}}`,
			[]string{"[Text editor: file updated]"}},
		{"text editor replace",
			`{"type":"text_editor_code_execution_tool_result","tool_use_id":"e","content":{"type":"text_editor_code_execution_str_replace_result","lines":["-old","+new"]}}`,
			[]string{"[Text editor: text replaced]\n-old\n+new"}},
		{"text editor error",
			`{"type":"text_editor_code_execution_tool_result","tool_use_id":"e","content":{"type":"text_editor_code_execution_tool_result_error","error_code":"file_not_found","error_message":"no such file"}}`,
			[]string{"[Text editor failed: file_not_found: no such file]"}},
		{"tool search result",
			`{"type":"tool_search_tool_result","tool_use_id":"ts","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}}`,
			[]string{"[Tool search found: get_weather]"}},
		{"mcp tool use and result",
			`{"type":"mcp_tool_use","id":"mcptoolu_1","name":"echo","server_name":"demo","input":{"text":"hi"}},` +
				`{"type":"mcp_tool_result","tool_use_id":"mcptoolu_1","is_error":false,"content":[{"type":"text","text":"hi back"}]}`,
			[]string{"[MCP tool call: demo/echo {\"text\":\"hi\"}]", "[MCP tool result]\nhi back"}},
		{"mcp tool error with string content",
			`{"type":"mcp_tool_result","tool_use_id":"m","is_error":true,"content":"boom"}`,
			[]string{"[MCP tool error]\nboom"}},
		{"compaction with readable summary",
			`{"type":"compaction","content":"User wants a CSV parser."}`,
			[]string{"[Summary of the earlier conversation]\nUser wants a CSV parser."}},
		{"image in assistant turn",
			`{"type":"text","text":"see"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`,
			[]string{"see\n\n[image omitted]"}},
		{"document in assistant turn",
			`{"type":"document","source":{"type":"text","data":"doc body"}}`,
			[]string{"[Document]\ndoc body"}},
		{"unknown future block",
			`{"type":"hologram","data":"x"}`,
			[]string{"[hologram content omitted]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, msgs := ccMsgs(t, "claude", assistantMsg(c.blocks))
			if len(msgs) != 3 || msgs[1]["role"] != "assistant" || msgs[2]["content"] != "next" {
				t.Fatalf("messages = %v", msgs)
			}
			wantAll(t, textOf(msgs[1]), c.want...)
			wantNone(t, textOf(msgs[1]), "encrypted", "EqgfCio")
		})
	}
}

func TestSanitizeClaudeAssistantKeepsToolCallsAndDropsEncryptedBlocks(t *testing.T) {
	_, msgs := ccMsgs(t, "claude", `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"compaction","encrypted_content":"opaque"},`+
		`{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"x"}},`+
		`{"type":"tool_use","id":"toolu_9","name":"Bash","input":{"command":"ls"}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":"a.txt"}]}]}`)
	a := msgs[1]
	calls, _ := a["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "toolu_9" {
		t.Fatalf("tool call lost: %v", a)
	}
	if got := textOf(a); got != `[Server tool call: web_search {"query":"x"}]` {
		t.Fatalf("assistant text = %q", got)
	}
	if msgs[2]["role"] != "tool" || msgs[2]["content"] != "a.txt" {
		t.Fatalf("tool reply = %v", msgs[2])
	}
}

func TestSanitizeClaudeToolResultBlocks(t *testing.T) {
	cases := []struct {
		name, content string
		want          []string
	}{
		{"PDF document read by Claude Code",
			`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdfB64("Quarterly revenue: 4.2M", "Headcount: 37") + `"}}`,
			[]string{"[Document (PDF, 1 page)]", "Quarterly revenue: 4.2M\nHeadcount: 37"}},
		{"text around a document",
			`{"type":"text","text":"PDF file read: /tmp/a.pdf"},{"type":"document","source":{"type":"text","data":"inner"}},{"type":"text","text":"done"}`,
			[]string{"PDF file read: /tmp/a.pdf\n\n[Document]\ninner\n\ndone"}},
		{"search result",
			`{"type":"search_result","source":"doc://1","title":"Hit","content":[{"type":"text","text":"snippet"}]}`,
			[]string{"[Search result: Hit]\nSource: doc://1\nsnippet"}},
		{"image with file source",
			`{"type":"image","source":{"type":"file","file_id":"file_z"}}`,
			[]string{"[image omitted: uploaded file file_z is not available here]"}},
		{"unknown block",
			`{"type":"audio","data":"x"}`,
			[]string{"[audio content omitted]"}},
		{"tool reference still supported",
			`{"type":"tool_reference","tool_name":"web_search"},{"type":"document","source":{"type":"text","data":"d"}}`,
			[]string{"[tool: web_search]\n\n[Document]\nd"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, msgs := ccMsgs(t, "claude", toolResultMsg(c.content))
			tool := msgs[2]
			if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" {
				t.Fatalf("tool message = %v", msgs)
			}
			wantAll(t, tool["content"].(string), c.want...)
		})
	}
}

func TestSanitizeClaudeToolResultKeepsImagesBesideDocument(t *testing.T) {
	_, msgs := ccMsgs(t, "claude", toolResultMsg(
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},`+
			`{"type":"document","source":{"type":"text","data":"notes"}}`))
	wantAll(t, msgs[2]["content"].(string), "[Document]\nnotes", "tool_call_id toolu_1")
	if len(msgs) != 4 || len(imageURLs(msgs[3])) != 1 {
		t.Fatalf("tool image not forwarded: %v", msgs)
	}
}

func TestSanitizeClaudeSystemBlocks(t *testing.T) {
	_, msgs := ccMsgs(t, "claude", `{"system":[{"type":"text","text":"You are helpful."},`+
		`{"type":"document","title":"Policy","source":{"type":"text","data":"Be brief."}},`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},{"type":"widget"}],`+
		`"messages":[{"role":"system","content":[{"type":"text","text":"Extra rule."},{"type":"search_result","source":"s","title":"T","content":[{"type":"text","text":"c"}]},`+
		`{"type":"image","source":{"type":"url","url":"https://x/i.png"}}]},{"role":"user","content":"hi"}]}`)
	if len(msgs) != 3 || msgs[0]["role"] != "system" || msgs[1]["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
	if got := msgs[0]["content"]; got != "You are helpful.\n\n[Document: Policy]\nBe brief.\n\n[image omitted]\n\n[widget content omitted]" {
		t.Fatalf("top-level system = %q", got)
	}
	if got := msgs[1]["content"]; got != "Extra rule.\n\n[Search result: T]\nSource: s\nc\n\n[image omitted]" {
		t.Fatalf("system turn = %q", got)
	}
}

func TestSanitizeClaudeHostedTools(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"web_search"},"tools":[` +
		`{"type":"web_search_20250305","name":"web_search","max_uses":5},` +
		`{"type":"web_fetch_20250910","name":"web_fetch"},` +
		`{"type":"code_execution_20250825","name":"code_execution"},` +
		`{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},` +
		`{"type":"mcp_toolset","mcp_server_name":"demo"},` +
		`{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}},` +
		`{"type":"custom","name":"lookup","input_schema":{"type":"object"}},` +
		`{"type":"bash_20250124","name":"bash"},` +
		`{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}]}`
	m, _ := ccMsgs(t, "claude", body)
	var names []string
	for _, tl := range m["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "Bash,lookup,bash,str_replace_based_edit_tool" {
		t.Fatalf("tools = %s", got)
	}
	if m["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto (named tool was removed)", m["tool_choice"])
	}

	// Only hosted tools: no tools left, so no tool_choice either.
	m, _ = ccMsgs(t, "claude", `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"any"},`+
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	if _, ok := m["tools"]; ok {
		t.Fatalf("tools = %v, want none", m["tools"])
	}
	if _, ok := m["tool_choice"]; ok {
		t.Fatalf("tool_choice = %v, want none", m["tool_choice"])
	}

	// A named choice for a kept tool is untouched.
	m, _ = ccMsgs(t, "claude", `{"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"Bash"},`+
		`"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"Bash","input_schema":{"type":"object"}}]}`)
	if tc, _ := json.Marshal(m["tool_choice"]); string(tc) != `{"function":{"name":"Bash"},"type":"function"}` {
		t.Fatalf("tool_choice = %s", tc)
	}
}

// TestSanitizeClaudeWebSearchHistoryScenario replays a conversation that
// started on real Claude with server-side web search and continues on a
// Chat Completions model.
func TestSanitizeClaudeWebSearchHistoryScenario(t *testing.T) {
	body := `{
	  "model": "claude-sonnet-5-5",
	  "max_tokens": 4096,
	  "system": [{"type":"text","text":"You are a research assistant.","cache_control":{"type":"ephemeral"}}],
	  "tools": [
	    {"type":"web_search_20250305","name":"web_search","max_uses":5},
	    {"name":"save_note","description":"Save a note","input_schema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}
	  ],
	  "messages": [
	    {"role":"user","content":"When is the next Go release and what is new?"},
	    {"role":"assistant","content":[
	      {"type":"text","text":"I'll search for the latest Go release information."},
	      {"type":"server_tool_use","id":"srvtoolu_01WYG3ziw53XMcoyKL4XcZmE","name":"web_search","input":{"query":"Go 1.27 release date features"}},
	      {"type":"web_search_tool_result","tool_use_id":"srvtoolu_01WYG3ziw53XMcoyKL4XcZmE","content":[
	        {"type":"web_search_result","url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes - The Go Programming Language","encrypted_content":"EqgfCioIARgBIiQ3YTAwMjY1Mi1mZjM5LTQ1NGUtODgxNC1kNjNjNTk1ZWI3Y2MSDBp","page_age":"August 12, 2026"},
	        {"type":"web_search_result","url":"https://github.com/golang/go/milestone/400","title":"Go1.27 milestone","encrypted_content":"EpYKCioIARgBIiRkYzQ4NmU0MC1jM2Q3LTQ0MTQtOTUxZS1iYmM0YTU1MjQzZTMSD","page_age":null}
	      ]},
	      {"type":"text","text":"Go 1.27 was released in August 2026","citations":[{"type":"web_search_result_location","url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes - The Go Programming Language","encrypted_index":"Eo8BCioIAhgBIiQyYjQ0OWJmZi1lNm","cited_text":"Go 1.27 is the latest Go release."}]},
	      {"type":"text","text":" and adds generic type aliases."},
	      {"type":"tool_use","id":"toolu_01A09q90qw90lq917835lq9","name":"save_note","input":{"text":"Go 1.27: Aug 2026"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_01A09q90qw90lq917835lq9","content":"saved"},
	      {"type":"text","text":"Thanks. Summarize that in one line."}
	    ]}
	  ]
	}`
	m, msgs := ccMsgs(t, "claude", body)
	if len(msgs) != 5 {
		t.Fatalf("got %d messages: %v", len(msgs), msgs)
	}
	asst := textOf(msgs[2])
	wantAll(t, asst,
		"I'll search for the latest Go release information.",
		`[Server tool call: web_search {"query":"Go 1.27 release date features"}]`,
		"1. Go 1.27 Release Notes - The Go Programming Language\n   https://go.dev/doc/go1.27\n   Page age: August 12, 2026",
		"2. Go1.27 milestone\n   https://github.com/golang/go/milestone/400",
		"Go 1.27 was released in August 2026 and adds generic type aliases.")
	wantNone(t, asst, "EqgfCio", "Eo8BCio", "encrypted")
	if calls, _ := msgs[2]["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("save_note call lost: %v", msgs[2])
	}
	if msgs[3]["role"] != "tool" || msgs[3]["content"] != "saved" || textOf(msgs[4]) != "Thanks. Summarize that in one line." {
		t.Fatalf("follow-up turn = %v / %v", msgs[3], msgs[4])
	}
	tools := m["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "save_note" {
		t.Fatalf("tools = %v, want only save_note", tools)
	}
}

// TestSanitizeClaudeCodeReadsPDFScenario replays Claude Code reading a PDF:
// the client believes it talks to Claude and sends the file itself as a
// document block inside the Read tool's result.
func TestSanitizeClaudeCodeReadsPDFScenario(t *testing.T) {
	pdf := pdfB64("Service Agreement", "Term: 12 months", "Fee: 3,000 USD per month")
	body := `{
	  "model": "claude-sonnet-5-5",
	  "max_tokens": 32000,
	  "stream": true,
	  "system": [{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
	  "tools": [{"name":"Read","description":"Reads a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"<system-reminder>context</system-reminder>"},{"type":"text","text":"What is the monthly fee in contract.pdf?"}]},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"I should read the file.","signature":"EuYBCkQYAiJA"},
	      {"type":"tool_use","id":"toolu_01PDF","name":"Read","input":{"file_path":"/work/contract.pdf"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_01PDF","content":[
	        {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + pdf + `"}}
	      ]}
	    ]},
	    {"role":"assistant","content":[{"type":"text","text":"The monthly fee is 3,000 USD."}]},
	    {"role":"user","content":"And the term?"}
	  ]
	}`
	_, msgs := ccMsgs(t, "claude", body)
	if len(msgs) != 6 {
		t.Fatalf("got %d messages: %v", len(msgs), msgs)
	}
	tool := msgs[3]
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_01PDF" {
		t.Fatalf("tool message = %v", tool)
	}
	wantAll(t, tool["content"].(string), "[Document (PDF, 1 page)]", "Service Agreement\nTerm: 12 months\nFee: 3,000 USD per month")
	wantNone(t, tool["content"].(string), pdf[:40])
	if textOf(msgs[5]) != "And the term?" {
		t.Fatalf("last turn = %v", msgs[5])
	}
}

// ---- Responses (Codex) ----

func respInput(items string) string {
	return `{"model":"x","instructions":"Be helpful.","input":[` + items + `]}`
}

func TestSanitizeResponsesItems(t *testing.T) {
	cases := []struct {
		name, items string
		want        []string // role:text of each message after the system message
	}{
		{"web search calls merge across reasoning",
			`{"type":"message","role":"user","content":"news?"},` +
				`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAA"},` +
				`{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"go 1.27","sources":[{"type":"url","url":"https://go.dev/doc/go1.27"}]}},` +
				`{"type":"reasoning","id":"rs_2","summary":[]},` +
				`{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"open_page","url":"https://go.dev/blog"}},` +
				`{"type":"web_search_call","id":"ws_3","status":"completed","action":{"type":"find","url":"https://go.dev/blog","pattern":"generics"}},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.27 is out."}]}`,
			[]string{"user:news?",
				"assistant:[Web search: go 1.27]\nSources:\n- https://go.dev/doc/go1.27\n\n[Opened web page: https://go.dev/blog]\n\n[Searched https://go.dev/blog for: generics]",
				"assistant:Go 1.27 is out."}},
		{"web search without action",
			`{"type":"web_search_call","id":"ws","status":"completed"}`,
			[]string{"assistant:[Web search]"}},
		{"local shell call and output",
			`{"type":"local_shell_call","id":"lsh","call_id":"c1","status":"completed","action":{"type":"exec","command":["ls","-la"],"working_directory":"/repo"}},` +
				`{"type":"local_shell_call_output","id":"lsh","call_id":"c1","output":"a.go\nb.go"}`,
			[]string{"assistant:[Local shell command: ls -la] (in /repo)", "user:[Local shell output]\na.go\nb.go"}},
		{"shell call and output",
			`{"type":"shell_call","call_id":"s1","action":{"commands":["go test ./...","git status"]}},` +
				`{"type":"shell_call_output","call_id":"s1","output":[{"stdout":"ok","stderr":"warn","outcome":{"type":"exit","exit_code":1}},{"stdout":"","outcome":{"type":"timeout"}}]}`,
			[]string{"assistant:[Shell command]\n$ go test ./...\n$ git status", "user:[Shell output]\nok\nstderr:\nwarn\n(exit code 1)\n(timed out)"}},
		{"apply_patch call and output",
			`{"type":"apply_patch_call","call_id":"p1","status":"completed","operation":{"type":"update_file","path":"main.go","diff":"@@\n-a\n+b"}},` +
				`{"type":"apply_patch_call_output","call_id":"p1","status":"completed","output":"Done"}`,
			[]string{"assistant:[apply_patch: update_file main.go]\n@@\n-a\n+b", "user:[apply_patch result: completed]\nDone"}},
		{"computer use",
			`{"type":"computer_call","call_id":"cc","action":{"type":"click","x":10,"y":20},"pending_safety_checks":[]},` +
				`{"type":"computer_call_output","call_id":"cc","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,QUJD"}}`,
			[]string{`assistant:[Computer action: {"type":"click","x":10,"y":20}]`, "user:[Computer screenshot omitted]"}},
		{"image generation",
			`{"type":"image_generation_call","id":"ig","status":"completed","revised_prompt":"a red fox","result":"iVBORw0KGgoAAAANSUhEUg"}`,
			[]string{"assistant:[Generated an image: a red fox]"}},
		{"code interpreter",
			`{"type":"code_interpreter_call","id":"ci","container_id":"cntr","code":"print(2+2)","outputs":[{"type":"logs","logs":"4"},{"type":"image","url":"https://x/p.png"}]}`,
			[]string{"assistant:[Code interpreter]\nprint(2+2)\nOutput:\n4\n[output image omitted]"}},
		{"file search",
			`{"type":"file_search_call","id":"fs","queries":["refund policy"],"results":[{"file_id":"f1","filename":"policy.md","text":"Refunds within 30 days","score":0.9}]}`,
			[]string{"assistant:[File search: refund policy]\n1. policy.md: Refunds within 30 days"}},
		{"mcp items",
			`{"type":"mcp_list_tools","id":"ml","server_label":"deepwiki","tools":[{"name":"ask","input_schema":{}},{"name":"read","input_schema":{}}]},` +
				`{"type":"mcp_approval_request","id":"ar","server_label":"deepwiki","name":"ask","arguments":"{\"q\":\"x\"}"},` +
				`{"type":"mcp_approval_response","approval_request_id":"ar","approve":true},` +
				`{"type":"mcp_call","id":"mc","server_label":"deepwiki","name":"ask","arguments":"{\"q\":\"x\"}","output":"answer"},` +
				`{"type":"mcp_call","id":"mc2","server_label":"deepwiki","name":"read","arguments":"{}","error":"timeout"}`,
			[]string{"assistant:[MCP server deepwiki tools: ask, read]\n\n[MCP approval requested: deepwiki/ask {\"q\":\"x\"}]",
				"user:[MCP approval: approved]",
				"assistant:[MCP tool call: deepwiki/ask {\"q\":\"x\"}]\nanswer\n\n[MCP tool call: deepwiki/read {}]\nError: timeout"}},
		{"item reference dropped",
			`{"type":"message","role":"user","content":"a"},{"type":"item_reference","id":"msg_123"},{"type":"message","role":"user","content":"b"}`,
			[]string{"user:a", "user:b"}},
		{"unknown future items",
			`{"type":"hologram_call","id":"h"},{"type":"hologram_call_output","call_id":"h"}`,
			[]string{"assistant:[hologram_call item omitted]", "user:[hologram_call_output item omitted]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, msgs := ccMsgs(t, "openai-response", respInput(c.items))
			if msgs[0]["role"] != "system" || msgs[0]["content"] != "Be helpful." {
				t.Fatalf("system = %v", msgs[0])
			}
			var got []string
			for _, m := range msgs[1:] {
				got = append(got, m["role"].(string)+":"+textOf(m))
			}
			if g, w := strings.Join(got, "\n---\n"), strings.Join(c.want, "\n---\n"); g != w {
				t.Fatalf("messages:\n%s\nwant:\n%s", g, w)
			}
			wantNone(t, strings.Join(got, "\n"), "gAAAA", "iVBORw0K", "QUJD")
		})
	}
}

func TestSanitizeResponsesCompaction(t *testing.T) {
	_, msgs := ccMsgs(t, "openai-response", respInput(
		`{"type":"compaction","encrypted_content":"gAAAAABo-encrypted"},{"type":"message","role":"user","content":"continue"}`))
	if len(msgs) != 2 || msgs[1]["content"] != "continue" {
		t.Fatalf("messages = %v", msgs)
	}
	wantAll(t, msgs[0]["content"].(string), "Be helpful.\n\n[Note: part of the earlier conversation was compacted")
	wantNone(t, msgs[0]["content"].(string), "gAAAA")

	// Without instructions the note becomes the system message.
	_, msgs = ccMsgs(t, "openai-response", `{"input":[{"type":"compaction_summary","encrypted_content":"x"},{"type":"message","role":"user","content":"go"}]}`)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || !strings.HasPrefix(msgs[0]["content"].(string), "[Note: part of the earlier") {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestSanitizeResponsesMessageParts(t *testing.T) {
	pdf := pdfB64("Invoice 42", "Total: 99 USD")
	cases := []struct {
		name, item, role string
		want             []string
	}{
		{"input_file PDF", `{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"},{"type":"input_file","filename":"invoice.pdf","file_data":"data:application/pdf;base64,` + pdf + `"}]}`,
			"user", []string{"summarize", "[Document: invoice.pdf (PDF, 1 page)]\nInvoice 42\nTotal: 99 USD"}},
		{"input_file by id", `{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-abc"}]}`,
			"user", []string{"(The document is an uploaded file (file-abc) that is not available here.)"}},
		{"image by file id", `{"type":"message","role":"user","content":[{"type":"input_text","text":"see"},{"type":"input_image","file_id":"file-img"}]}`,
			"user", []string{"see", "[image omitted: uploaded file file-img is not available here]"}},
		{"audio part", `{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}`,
			"user", []string{"[input_audio content omitted]"}},
		{"refusal", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Sorry."},{"type":"refusal","refusal":"I cannot help with that."}]}`,
			"assistant", []string{"Sorry.\n\n[Refusal] I cannot help with that."}},
		{"system image", `{"type":"message","role":"developer","content":[{"type":"input_text","text":"Rules."},{"type":"input_image","image_url":"https://x/i.png"}]}`,
			"system", []string{"Rules.\n\n[image omitted]"}},
		{"unknown part", `{"type":"message","role":"user","content":[{"type":"input_video","url":"https://x/v.mp4"}]}`,
			"user", []string{"[input_video content omitted]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, msgs := ccMsgs(t, "openai-response", `{"input":[`+c.item+`]}`)
			if len(msgs) != 1 || msgs[0]["role"] != c.role {
				t.Fatalf("messages = %v", msgs)
			}
			wantAll(t, textOf(msgs[0]), c.want...)
		})
	}
}

func TestSanitizeResponsesToolOutputImagesAndFiles(t *testing.T) {
	// Codex view_image: two parallel calls, the first returns an image.
	_, msgs := ccMsgs(t, "openai-response", `{"input":[`+
		`{"type":"message","role":"user","content":"look at both"},`+
		`{"type":"function_call","call_id":"call_a","name":"view_image","arguments":"{\"path\":\"a.png\"}"},`+
		`{"type":"function_call","call_id":"call_b","name":"shell","arguments":"{\"cmd\":\"ls\"}"},`+
		`{"type":"function_call_output","call_id":"call_a","output":[{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]},`+
		`{"type":"function_call_output","call_id":"call_b","output":"a.png"},`+
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	if len(msgs) != 6 {
		t.Fatalf("got %d messages: %v", len(msgs), msgs)
	}
	if calls, _ := msgs[1]["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("tool calls = %v", msgs[1])
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_a" ||
		msgs[2]["content"] != "[Images from this tool result are in the following user message, labeled with tool_call_id call_a.]" {
		t.Fatalf("image tool output = %v", msgs[2])
	}
	if msgs[3]["role"] != "tool" || msgs[3]["content"] != "a.png" {
		t.Fatalf("second tool output = %v (tool replies must stay adjacent)", msgs[3])
	}
	if msgs[4]["role"] != "user" || textOf(msgs[4]) != "Tool result images for tool_call_id call_a:" ||
		len(imageURLs(msgs[4])) != 1 || imageURLs(msgs[4])[0] != "data:image/png;base64,QUJD" {
		t.Fatalf("image message = %v", msgs[4])
	}
	if textOf(msgs[5]) != "done" {
		t.Fatalf("last = %v", msgs[5])
	}

	// Files become text; images known only by file id become markers.
	_, msgs = ccMsgs(t, "openai-response", `{"input":[`+
		`{"type":"function_call","call_id":"c","name":"fetch","arguments":"{}"},`+
		`{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"fetched:"},`+
		`{"type":"input_file","filename":"spec.pdf","file_data":"data:application/pdf;base64,`+pdfB64("Spec v2")+`"},`+
		`{"type":"input_image","file_id":"file-9"}]}]}`)
	if len(msgs) != 2 || msgs[1]["role"] != "tool" {
		t.Fatalf("messages = %v", msgs)
	}
	if got := msgs[1]["content"]; got != "fetched:\n\n[Document: spec.pdf (PDF, 1 page)]\nSpec v2\n\n[image omitted: uploaded file file-9 is not available here]" {
		t.Fatalf("tool output = %q", got)
	}
}

func TestSanitizeResponsesHostedTools(t *testing.T) {
	body := `{"input":[{"type":"message","role":"user","content":"hi"},` +
		`{"type":"additional_tools","tools":[{"type":"file_search","vector_store_ids":["vs"]},{"type":"function","name":"extra"}]}],` +
		`"tool_choice":{"type":"web_search_preview"},"tools":[` +
		`{"type":"function","name":"shell","parameters":{"type":"object"}},` +
		`{"type":"custom","name":"grep_tool","format":{"type":"text"}},` +
		`{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"echo"},{"type":"file_search","vector_store_ids":["vs"]}]},` +
		`{"type":"namespace","name":"hosted_only","tools":[{"type":"code_interpreter","container":{"type":"auto"}}]},` +
		`{"type":"web_search"},{"type":"web_search_preview"},{"type":"local_shell"},{"type":"file_search","vector_store_ids":["vs"]},` +
		`{"type":"mcp","server_label":"x","server_url":"https://x"},{"type":"computer_use_preview","display_width":1,"display_height":1},` +
		`{"type":"code_interpreter","container":{"type":"auto"}},{"type":"image_generation"},{"type":"shell"},{"type":"apply_patch"}]}`
	m, _ := ccMsgs(t, "openai-response", body)
	var names []string
	for _, tl := range m["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "shell,grep_tool,mcp__echo,extra" {
		t.Fatalf("tools = %s", got)
	}
	if m["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto for a hosted tool type", m["tool_choice"])
	}

	for _, c := range []struct{ choice, want string }{
		{`{"type":"custom","name":"grep_tool"}`, `{"function":{"name":"grep_tool"},"type":"function"}`},
		{`{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"shell"}]}`, `"required"`},
		{`{"type":"allowed_tools","mode":"auto","tools":[]}`, `"auto"`},
		{`{"type":"mcp","server_label":"x"}`, `"auto"`},
		{`"required"`, `"required"`},
	} {
		m, _ := ccMsgs(t, "openai-response", `{"input":"hi","tool_choice":`+c.choice+`,"tools":[{"type":"function","name":"shell"},{"type":"custom","name":"grep_tool"}]}`)
		if got, _ := json.Marshal(m["tool_choice"]); string(got) != c.want {
			t.Fatalf("tool_choice %s -> %s, want %s", c.choice, got, c.want)
		}
	}

	// Only hosted tools: nothing left to choose from.
	m, _ = ccMsgs(t, "openai-response", `{"input":"hi","tool_choice":"required","tools":[{"type":"web_search"},{"type":"local_shell"}]}`)
	if _, ok := m["tools"]; ok {
		t.Fatalf("tools = %v", m["tools"])
	}
	if _, ok := m["tool_choice"]; ok {
		t.Fatalf("tool_choice = %v, want none", m["tool_choice"])
	}
}

// TestSanitizeCodexHistoryScenario replays a Codex session that ran on a
// GPT model (web search, view_image, a compaction, encrypted reasoning,
// a hosted tool) and continues on a Chat Completions model.
func TestSanitizeCodexHistoryScenario(t *testing.T) {
	body := `{
	  "model": "gpt-5.5-codex",
	  "instructions": "You are Codex, a coding agent.",
	  "stream": true,
	  "store": false,
	  "include": ["reasoning.encrypted_content"],
	  "prompt_cache_key": "019a-session",
	  "reasoning": {"effort": "high", "summary": "auto"},
	  "tools": [
	    {"type":"function","name":"shell","description":"Runs a shell command","strict":false,"parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}},"required":["command"]}},
	    {"type":"function","name":"view_image","description":"Attach a local image","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
	    {"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}},
	    {"type":"web_search"}
	  ],
	  "tool_choice": "auto",
	  "parallel_tool_calls": false,
	  "input": [
	    {"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>sandbox: workspace-write</permissions instructions>"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/repo</cwd></environment_context>"}]},
	    {"type":"compaction","encrypted_content":"gAAAAABpY2NvbXBhY3RlZC1oaXN0b3J5"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"Why does the screenshot show a 500? Check the docs too."}]},
	    {"type":"reasoning","id":"rs_01","summary":[{"type":"summary_text","text":"Look at the screenshot."}],"encrypted_content":"gAAAAABreasoning1"},
	    {"type":"function_call","id":"fc_01","call_id":"call_img","name":"view_image","arguments":"{\"path\":\"/repo/err.png\"}"},
	    {"type":"function_call_output","call_id":"call_img","output":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]},
	    {"type":"reasoning","id":"rs_02","summary":[],"encrypted_content":"gAAAAABreasoning2"},
	    {"type":"web_search_call","id":"ws_01","status":"completed","action":{"type":"search","query":"nginx 500 upstream prematurely closed connection"}},
	    {"type":"message","role":"assistant","id":"msg_01","content":[{"type":"output_text","text":"The upstream closed the connection early; raise proxy_read_timeout.","annotations":[]}]},
	    {"type":"custom_tool_call","id":"ctc_01","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
	    {"type":"custom_tool_call_output","call_id":"call_patch","output":"Success."},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"Thanks, now run the tests."}]}
	  ]
	}`
	m, msgs := ccMsgs(t, "openai-response", body)
	var roles []string
	for _, msg := range msgs {
		roles = append(roles, msg["role"].(string))
	}
	if got := strings.Join(roles, ","); got != "system,system,user,user,assistant,tool,user,assistant,assistant,assistant,tool,user" {
		t.Fatalf("roles = %s", got)
	}
	wantAll(t, msgs[0]["content"].(string), "You are Codex", "[Note: part of the earlier conversation was compacted")
	wantAll(t, textOf(msgs[3]), "Why does the screenshot show a 500?")
	if msgs[5]["tool_call_id"] != "call_img" || !strings.Contains(msgs[5]["content"].(string), "tool_call_id call_img") {
		t.Fatalf("view_image output = %v", msgs[5])
	}
	if urls := imageURLs(msgs[6]); len(urls) != 1 || urls[0] != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("image message = %v", msgs[6])
	}
	wantAll(t, textOf(msgs[7]), "[Web search: nginx 500 upstream prematurely closed connection]")
	wantAll(t, textOf(msgs[8]), "raise proxy_read_timeout")
	if calls, _ := msgs[9]["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("apply_patch call = %v", msgs[9])
	}
	if msgs[10]["tool_call_id"] != "call_patch" || msgs[10]["content"] != "Success." || textOf(msgs[11]) != "Thanks, now run the tests." {
		t.Fatalf("tail = %v / %v", msgs[10], msgs[11])
	}
	all, _ := json.Marshal(msgs)
	wantNone(t, string(all), "gAAAA", "encrypted_content", "summary_text")
	var names []string
	for _, tl := range m["tools"].([]any) {
		names = append(names, tl.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "shell,view_image" {
		t.Fatalf("tools = %s", got)
	}
}
