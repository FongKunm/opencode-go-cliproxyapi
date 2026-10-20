package sanitize

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ClaudeMessages rewrites an Anthropic Messages request body for the Chat
// Completions translator (chatcompletions.claudeToChat):
//
//   - blocks it cannot represent (documents, search results, server tool
//     calls and results, MCP blocks, container uploads, unknown future
//     types) become text in place, in user and assistant turns, in
//     role:"system" turns, in the top-level system field and inside
//     tool_result content;
//   - images survive where the translator renders them (user content and
//     tool results with base64 or url sources) and become a marker
//     elsewhere; reasoning blocks outside assistant turns are dropped;
//   - hosted tools (any typed tool except "custom" and the client-executed
//     bash/text_editor/computer/memory tools) are removed, and a
//     tool_choice naming a removed tool falls back to auto.
//
// The original body is returned unchanged when nothing needs rewriting or
// when it is not a JSON object.
func ClaudeMessages(body []byte) []byte {
	req, ok := object(body)
	if !ok {
		return body
	}
	changed := false
	if out, ok := claudeSystem(req["system"]); ok {
		req["system"] = out
		changed = true
	}
	if out, ok := claudeMessages(req["messages"]); ok {
		req["messages"] = out
		changed = true
	}
	if out, removed, kept, ok := claudeTools(req["tools"]); ok {
		req["tools"] = out
		changed = true
		if tc, has := req["tool_choice"]; has {
			switch out, drop := claudeToolChoice(tc, removed, kept); {
			case drop:
				delete(req, "tool_choice")
			case out != nil:
				req["tool_choice"] = out
			}
		}
	}
	if !changed {
		return body
	}
	return encodeOr(req, body)
}

// claudeSystem rewrites a block-array system field. The translator joins
// system text blocks with blank lines, so rewritten blocks need no
// separators of their own.
func claudeSystem(raw json.RawMessage) (json.RawMessage, bool) {
	blocks, ok := array(raw)
	if !ok {
		return nil, false
	}
	pieces := make([]piece, 0, len(blocks))
	changed := false
	for _, blk := range blocks {
		if t, isObj := kindOf(blk); !isObj || t == "text" {
			pieces = append(pieces, keep(blk))
			continue
		}
		changed = true
		for _, p := range renderClaude(blk, false) {
			if p.text != "" {
				pieces = append(pieces, p)
			}
		}
	}
	if !changed {
		return nil, false
	}
	return assemble(pieces, false, "text"), true
}

func claudeMessages(raw json.RawMessage) (json.RawMessage, bool) {
	msgs, ok := array(raw)
	if !ok {
		return nil, false
	}
	changed := false
	for i, msg := range msgs {
		roleRaw, _ := field(msg, "role")
		role, _ := decodeString(roleRaw)
		raw, _ := field(msg, "content")
		content, ok := claudeContent(raw, role)
		if !ok {
			continue
		}
		m, ok := object(msg)
		if !ok {
			continue // malformed turn: the translator reports it
		}
		m["content"] = content
		msgs[i] = marshal(m)
		changed = true
	}
	if !changed {
		return nil, false
	}
	return marshal(msgs), true
}

// claudeContent rewrites one turn's block array. User blocks become
// separate content parts upstream; assistant and system turns are joined
// into one string, so their rewritten text gets separators.
func claudeContent(raw json.RawMessage, role string) (json.RawMessage, bool) {
	if role != "user" && role != "assistant" && role != "system" {
		return nil, false // the translator reports unknown roles
	}
	blocks, ok := array(raw)
	if !ok {
		return nil, false
	}
	pieces := make([]piece, 0, len(blocks))
	changed := false
	for _, blk := range blocks {
		t, isObj := kindOf(blk)
		switch {
		case !isObj:
			pieces = append(pieces, keep(blk))
		case t == "text":
			pieces = append(pieces, origText(blk))
		case t == "image" && role == "user" && imageKeep(blk):
			pieces = append(pieces, keep(blk))
		case t == "tool_result" && role == "user":
			if out, ok := claudeToolResult(blk); ok {
				pieces = append(pieces, keep(out))
				changed = true
			} else {
				pieces = append(pieces, keep(blk))
			}
		case role == "assistant" && (t == "thinking" || t == "redacted_thinking" || t == "tool_use"):
			pieces = append(pieces, keep(blk))
		case t == "thinking" || t == "redacted_thinking":
			changed = true // reasoning outside an assistant turn has no place to go
		default:
			pieces = append(pieces, renderClaude(blk, role == "user")...)
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	return assemble(pieces, role != "user", "text"), true
}

// claudeToolResult rewrites the block array of a tool_result. The
// translator joins its text parts without a separator and keeps images.
func claudeToolResult(raw json.RawMessage) (json.RawMessage, bool) {
	content, _ := field(raw, "content")
	blocks, ok := array(content)
	if !ok {
		return nil, false
	}
	pieces := make([]piece, 0, len(blocks))
	changed := false
	for _, blk := range blocks {
		t, isObj := kindOf(blk)
		switch {
		case !isObj:
			pieces = append(pieces, keep(blk))
		case t == "text":
			pieces = append(pieces, origText(blk))
		case t == "tool_reference":
			// The translator renders it as text; count it as text so
			// rewritten neighbours are separated from it.
			pieces = append(pieces, piece{raw: blk, text: toolReferenceText(blk), hasText: true, isText: true})
		case t == "image" && imageKeep(blk):
			pieces = append(pieces, keep(blk))
		default:
			pieces = append(pieces, renderClaude(blk, true)...)
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	m, ok := object(raw)
	if !ok {
		return nil, false // malformed block: the translator reports it
	}
	m["content"] = assemble(pieces, true, "text")
	return marshal(m), true
}

// renderClaude converts one block the translator cannot take at its
// position. allowImages reports whether image blocks may be emitted.
func renderClaude(raw json.RawMessage, allowImages bool) []piece {
	m, _ := object(raw)
	t := typeOf(raw)
	one := func(s string) []piece { return []piece{synthText(s)} }
	switch t {
	case "document":
		return claudeDocumentPieces(m, allowImages)
	case "image":
		return one(imageMarker(raw))
	case "search_result":
		return one(searchResultText(m))
	case "web_search_tool_result":
		return one(webSearchResultText(m))
	case "web_fetch_tool_result":
		return one(webFetchResultText(m))
	case "code_execution_tool_result", "bash_code_execution_tool_result":
		return one(codeExecutionText(m))
	case "text_editor_code_execution_tool_result":
		return one(textEditorText(m))
	case "tool_search_tool_result":
		return one(toolSearchText(m))
	case "server_tool_use":
		return one("[Server tool call: " + str(m, "name") + " " + compact(m["input"]) + "]")
	case "mcp_tool_use":
		return one("[MCP tool call: " + joinNonEmpty("/", str(m, "server_name"), str(m, "name")) + " " + compact(m["input"]) + "]")
	case "mcp_tool_result":
		return one(mcpResultText(m))
	case "container_upload":
		return one("[File uploaded to the code execution container: " + str(m, "file_id") + "]")
	case "compaction":
		if s := str(m, "content"); s != "" {
			return one("[Summary of the earlier conversation]\n" + s)
		}
		return nil // encrypted: nothing readable to keep
	case "tool_use":
		return one("[Tool call: " + str(m, "name") + " " + compact(m["input"]) + "]")
	case "tool_result":
		return one(toolResultText(m))
	case "tool_reference":
		return one(toolReferenceText(raw))
	case "":
		if s := str(m, "text"); s != "" {
			return one(s)
		}
	}
	return one(omitted(t))
}

// toolReferenceText is how the translator renders a tool_reference block
// (shared.ToolResultText).
func toolReferenceText(raw json.RawMessage) string {
	m, _ := object(raw)
	if name := str(m, "tool_name"); name != "" {
		return "[tool: " + name + "]"
	}
	return "[tool reference]"
}

// imageKeep reports whether an image block may stay where images are
// allowed: its source is one the translator renders (base64 or url), or
// the block is malformed and the translator should keep reporting it.
// Other source types (file references, unknown kinds) are valid but
// unsupported.
func imageKeep(raw json.RawMessage) bool {
	srcRaw, _ := field(raw, "source")
	src, ok := object(srcRaw)
	if !ok {
		return true
	}
	switch str(src, "type") {
	case "base64", "url":
		return true
	}
	return false
}

// imageUsable reports whether an image block has a renderable source.
func imageUsable(raw json.RawMessage) bool {
	m, _ := object(raw)
	src, _ := object(m["source"])
	switch str(src, "type") {
	case "base64":
		return str(src, "data") != ""
	case "url":
		return str(src, "url") != ""
	}
	return false
}

func imageMarker(raw json.RawMessage) string {
	m, _ := object(raw)
	src, _ := object(m["source"])
	switch t := str(src, "type"); t {
	case "file":
		return "[image omitted: uploaded file " + str(src, "file_id") + " is not available here]"
	case "base64", "url", "":
		return "[image omitted]"
	default:
		return "[image omitted: unsupported source " + t + "]"
	}
}

// textsOf returns the text of a content field that is a string or an
// array of text blocks; other blocks become markers.
func textsOf(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	blocks, _ := array(raw)
	var out []string
	for _, blk := range blocks {
		m, ok := object(blk)
		if !ok {
			continue
		}
		if t := str(m, "type"); t == "text" || t == "" {
			if s := str(m, "text"); s != "" {
				out = append(out, s)
			}
		} else {
			out = append(out, omitted(t))
		}
	}
	return out
}

func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func errorCode(m map[string]json.RawMessage) string {
	if c := str(m, "error_code"); c != "" {
		return c
	}
	return "unknown error"
}

func isError(m map[string]json.RawMessage) bool {
	return strings.HasSuffix(str(m, "type"), "_error")
}

func searchResultText(m map[string]json.RawMessage) string {
	var b strings.Builder
	b.WriteString("[Search result")
	if title := str(m, "title"); title != "" {
		b.WriteString(": " + title)
	}
	b.WriteString("]")
	if src := str(m, "source"); src != "" {
		b.WriteString("\nSource: " + src)
	}
	for _, t := range textsOf(m["content"]) {
		b.WriteString("\n" + t)
	}
	return b.String()
}

func webSearchResultText(m map[string]json.RawMessage) string {
	if e, ok := object(m["content"]); ok {
		return "[Web search failed: " + errorCode(e) + "]"
	}
	results, _ := array(m["content"])
	var b strings.Builder
	n := 0
	for _, r := range results {
		rm, ok := object(r)
		if !ok {
			continue
		}
		n++
		b.WriteString("\n" + strconv.Itoa(n) + ". " + str(rm, "title"))
		if u := str(rm, "url"); u != "" {
			b.WriteString("\n   " + u)
		}
		if age := str(rm, "page_age"); age != "" {
			b.WriteString("\n   Page age: " + age)
		}
	}
	if n == 0 {
		return "[Web search returned no results]"
	}
	return "[Web search results]" + b.String()
}

func webFetchResultText(m map[string]json.RawMessage) string {
	c, _ := object(m["content"])
	if str(c, "type") != "web_fetch_result" {
		return "[Web fetch failed: " + errorCode(c) + "]"
	}
	head := "[Fetched " + str(c, "url")
	if at := str(c, "retrieved_at"); at != "" {
		head += " at " + at
	}
	head += "]"
	docm, ok := object(c["content"])
	if !ok {
		return head
	}
	return head + "\n" + claudeDocument(docm).render()
}

func codeExecutionText(m map[string]json.RawMessage) string {
	c, _ := object(m["content"])
	if isError(c) {
		return "[Code execution failed: " + errorCode(c) + "]"
	}
	var b strings.Builder
	b.WriteString("[Code execution result")
	if rc, ok := c["return_code"]; ok {
		b.WriteString(", exit code " + compact(rc))
	}
	b.WriteString("]")
	if s := str(c, "stdout"); s != "" {
		b.WriteString("\nstdout:\n" + s)
	}
	if s := str(c, "stderr"); s != "" {
		b.WriteString("\nstderr:\n" + s)
	}
	files, _ := array(c["content"])
	var ids []string
	for _, f := range files {
		if fm, ok := object(f); ok && str(fm, "file_id") != "" {
			ids = append(ids, str(fm, "file_id"))
		}
	}
	if len(ids) > 0 {
		b.WriteString("\nOutput files: " + strings.Join(ids, ", "))
	}
	return b.String()
}

func textEditorText(m map[string]json.RawMessage) string {
	c, _ := object(m["content"])
	t := str(c, "type")
	switch {
	case isError(c):
		msg := errorCode(c)
		if em := str(c, "error_message"); em != "" {
			msg += ": " + em
		}
		return "[Text editor failed: " + msg + "]"
	case strings.HasSuffix(t, "_view_result"):
		return "[Text editor: viewed file]\n" + str(c, "content")
	case strings.HasSuffix(t, "_create_result"):
		var update bool
		json.Unmarshal(c["is_file_update"], &update)
		if update {
			return "[Text editor: file updated]"
		}
		return "[Text editor: file created]"
	case strings.HasSuffix(t, "_str_replace_result"):
		var lines []string
		json.Unmarshal(c["lines"], &lines)
		return strings.TrimRight("[Text editor: text replaced]\n"+strings.Join(lines, "\n"), "\n")
	}
	return omitted("text_editor_code_execution_tool_result")
}

func toolSearchText(m map[string]json.RawMessage) string {
	c, _ := object(m["content"])
	if isError(c) {
		return "[Tool search failed: " + errorCode(c) + "]"
	}
	refs, _ := array(c["tool_references"])
	var names []string
	for _, r := range refs {
		if rm, ok := object(r); ok && str(rm, "tool_name") != "" {
			names = append(names, str(rm, "tool_name"))
		}
	}
	if len(names) == 0 {
		return "[Tool search found no tools]"
	}
	return "[Tool search found: " + strings.Join(names, ", ") + "]"
}

func mcpResultText(m map[string]json.RawMessage) string {
	var isErr bool
	json.Unmarshal(m["is_error"], &isErr)
	label := "[MCP tool result]"
	if isErr {
		label = "[MCP tool error]"
	}
	if texts := textsOf(m["content"]); len(texts) > 0 {
		return label + "\n" + strings.Join(texts, "\n")
	}
	return label
}

func toolResultText(m map[string]json.RawMessage) string {
	label := "[Tool result"
	if id := str(m, "tool_use_id"); id != "" {
		label += " for " + id
	}
	label += "]"
	if texts := textsOf(m["content"]); len(texts) > 0 {
		return label + "\n" + strings.Join(texts, "\n")
	}
	return label
}

// clientToolPrefixes name Anthropic-defined tools the client executes;
// they stay (as functions), unlike tools Anthropic runs server-side.
var clientToolPrefixes = []string{"bash_", "text_editor_", "computer_", "memory_"}

// claudeTools drops hosted tool definitions. removed holds their names
// and kept counts the tools left.
func claudeTools(raw json.RawMessage) (out json.RawMessage, removed map[string]bool, kept int, changed bool) {
	tools, ok := array(raw)
	if !ok {
		return nil, nil, 0, false
	}
	keptTools := make([]json.RawMessage, 0, len(tools))
	for _, tl := range tools {
		m, ok := object(tl)
		t := str(m, "type")
		if !ok || t == "" || t == "custom" || hasPrefix(t, clientToolPrefixes) {
			keptTools = append(keptTools, tl)
			continue
		}
		if removed == nil {
			removed = make(map[string]bool)
		}
		removed[str(m, "name")] = true
	}
	if removed == nil {
		return nil, nil, 0, false
	}
	return marshal(keptTools), removed, len(keptTools), true
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// claudeToolChoice adjusts tool_choice after hosted tools were removed:
// with no tools left it is dropped, and a choice naming a removed tool
// becomes auto. A nil out with drop false keeps the original.
func claudeToolChoice(raw json.RawMessage, removed map[string]bool, kept int) (out json.RawMessage, drop bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if kept == 0 {
		return nil, true
	}
	if m, ok := object(raw); ok && str(m, "type") == "tool" && removed[str(m, "name")] {
		return json.RawMessage(`{"type":"auto"}`), false
	}
	return nil, false
}
