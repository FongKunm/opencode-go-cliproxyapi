package sanitize

import (
	"encoding/json"
	"strconv"
	"strings"
)

// compactionNote replaces compaction items, whose content is encrypted for
// the model that wrote them.
const compactionNote = "[Note: part of the earlier conversation was compacted into an encrypted summary that cannot be read here, so it has been left out.]"

// Responses rewrites an OpenAI Responses request body for the Chat
// Completions translator (chatcompletions.responsesToChat):
//
//   - input items it cannot represent (hosted tool calls such as
//     web_search_call, local/remote shell calls and their outputs,
//     apply_patch, computer use, image generation, code interpreter, file
//     search, MCP items, unknown future types) become short text messages
//     in place — calls as assistant text, outputs as user text, adjacent
//     ones merged;
//   - compaction items are dropped (their content is encrypted) and the
//     instructions say so; item_reference items are dropped;
//   - message parts it cannot take (input_file, file-only images, images
//     in system/developer messages, refusals, audio, unknown parts) become
//     text; function_call_output images move to a user message after the
//     run of tool outputs, the way Claude tool-result images do, and files
//     in tool outputs become text;
//   - hosted tool definitions (anything but function, custom and
//     namespace, also inside namespaces and additional_tools items) are
//     removed, and tool_choice is adjusted to what remains.
//
// The original body is returned unchanged when nothing needs rewriting or
// when it is not a JSON object.
func Responses(body []byte) []byte {
	req, ok := object(body)
	if !ok {
		return body
	}
	changed := false
	tools := toolTally{}
	if out, ok := respTools(req["tools"], &tools); ok {
		req["tools"] = out
		changed = true
	}
	compacted := false
	if items, ok := array(req["input"]); ok {
		rw := &respRewriter{tools: &tools}
		for _, item := range items {
			rw.item(item)
		}
		if rw.changed {
			req["input"] = rw.finish()
			changed = true
			compacted = rw.compacted
		}
	}
	if tc, has := req["tool_choice"]; has {
		switch out, drop := respToolChoice(tc, tools); {
		case drop:
			delete(req, "tool_choice")
			changed = true
		case out != nil:
			req["tool_choice"] = out
			changed = true
		}
	}
	if compacted {
		if out, ok := withNote(req["instructions"]); ok {
			req["instructions"] = out
		}
	}
	if !changed {
		return body
	}
	return encodeOr(req, body)
}

func withNote(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return marshal(compactionNote), true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, false // the translator reports non-string instructions
	}
	if s == "" {
		return marshal(compactionNote), true
	}
	return marshal(s + "\n\n" + compactionNote), true
}

// ---- tools ----

// toolTally counts the tool definitions kept and removed across tools and
// additional_tools items.
type toolTally struct {
	kept, removed int
}

// respTools drops hosted tool definitions from a tools array.
func respTools(raw json.RawMessage, tally *toolTally) (json.RawMessage, bool) {
	tools, ok := array(raw)
	if !ok {
		return nil, false
	}
	out := make([]json.RawMessage, 0, len(tools))
	changed := false
	for _, tl := range tools {
		m, isObj := object(tl)
		switch t := str(m, "type"); {
		case !isObj || t == "function" || t == "custom":
			out = append(out, tl)
			tally.kept++
		case t == "namespace":
			kept, ok := respNamespace(m, tally)
			if ok {
				changed = true
			}
			if kept != nil {
				out = append(out, kept)
			}
		default:
			tally.removed++
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	return marshal(out), true
}

// respNamespace drops hosted children of a namespace tool. It returns the
// (possibly rewritten) namespace, or nil when no child is left, and
// whether anything changed.
func respNamespace(m map[string]json.RawMessage, tally *toolTally) (json.RawMessage, bool) {
	children, ok := array(m["tools"])
	if !ok {
		tally.kept++
		return marshal(m), false // the translator reports a malformed namespace
	}
	kept := make([]json.RawMessage, 0, len(children))
	for _, c := range children {
		if t := typeOf(c); t == "function" || t == "custom" {
			kept = append(kept, c)
			tally.kept++
		} else {
			tally.removed++
		}
	}
	if len(kept) == len(children) {
		return marshal(m), false
	}
	if len(kept) == 0 {
		return nil, true
	}
	m["tools"] = marshal(kept)
	return marshal(m), true
}

// respToolChoice adjusts tool_choice: hosted tool types and named tools
// are only meaningful while that tool exists, custom tools are sent as
// functions, and allowed_tools keeps just its mode. With every tool
// removed the choice is dropped. A nil out with drop false keeps the
// original.
func respToolChoice(raw json.RawMessage, tally toolTally) (out json.RawMessage, drop bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if tally.removed > 0 && tally.kept == 0 {
		return nil, true
	}
	m, ok := object(raw)
	if !ok {
		return nil, false // string choices pass through
	}
	switch t := str(m, "type"); t {
	case "function", "auto", "none", "required", "any", "tool":
		return nil, false
	case "custom":
		return marshal(map[string]string{"type": "function", "name": str(m, "name")}), false
	case "allowed_tools":
		if str(m, "mode") == "required" {
			return marshal("required"), false
		}
		return marshal("auto"), false
	default: // a hosted tool type
		return marshal("auto"), false
	}
}

// ---- input items ----

// respRewriter rebuilds the input array. Rewritten calls and outputs
// become messages; consecutive ones of the same role are merged so the
// conversation does not fill up with one-line turns.
type respRewriter struct {
	tools     *toolTally
	out       []json.RawMessage
	synth     map[int]*synthMsg // synthesized messages by index in out
	last      int               // index of the open synthesized message, -1 when none
	images    []json.RawMessage // pending tool-output image parts
	changed   bool
	compacted bool
	started   bool
}

type synthMsg struct {
	role string
	text strings.Builder
}

func (r *respRewriter) init() {
	if !r.started {
		r.synth = make(map[int]*synthMsg)
		r.last = -1
		r.started = true
	}
}

// emit appends an item that produces a message upstream, after any
// pending tool images.
func (r *respRewriter) emit(item json.RawMessage) {
	r.flushImages()
	r.out = append(r.out, item)
	r.last = -1
}

// emitToolOutput appends a tool output; outputs of parallel calls must
// stay adjacent, so pending images wait for the end of the run.
func (r *respRewriter) emitToolOutput(item json.RawMessage, images []json.RawMessage) {
	r.out = append(r.out, item)
	r.images = append(r.images, images...)
	r.last = -1
}

// emitQuiet appends an item that produces no message (reasoning,
// additional_tools); it does not interrupt a run.
func (r *respRewriter) emitQuiet(item json.RawMessage) {
	r.out = append(r.out, item)
}

// emitText adds synthesized text as a message of role, merging it into
// the open synthesized message of the same role.
func (r *respRewriter) emitText(role, text string) {
	r.changed = true
	r.flushImages()
	if r.last >= 0 && r.synth[r.last].role == role {
		r.synth[r.last].text.WriteString("\n\n" + text)
		return
	}
	m := &synthMsg{role: role}
	m.text.WriteString(text)
	r.last = len(r.out)
	r.synth[r.last] = m
	r.out = append(r.out, nil) // filled in by finish
}

func (r *respRewriter) flushImages() {
	if len(r.images) == 0 {
		return
	}
	r.out = append(r.out, marshal(map[string]any{"type": "message", "role": "user", "content": r.images}))
	r.images = nil
	r.last = -1
}

func (r *respRewriter) finish() json.RawMessage {
	r.flushImages()
	for i, m := range r.synth {
		r.out[i] = marshal(map[string]string{"type": "message", "role": m.role, "content": m.text.String()})
	}
	if r.out == nil {
		r.out = []json.RawMessage{}
	}
	return marshal(r.out)
}

func (r *respRewriter) item(raw json.RawMessage) {
	r.init()
	m, ok := object(raw)
	if !ok {
		r.emit(raw) // the translator reports malformed items
		return
	}
	t := typeOf(raw)
	roleRaw, _ := field(raw, "role")
	role, _ := decodeString(roleRaw)
	if t == "" && strings.TrimSpace(role) != "" {
		t = "message"
	}
	switch t {
	case "message":
		content, _ := field(raw, "content")
		if content, ok := respContent(content, role); ok {
			m["content"] = content
			raw = marshal(m)
			r.changed = true
		}
		r.emit(raw)
	case "function_call", "custom_tool_call":
		r.emit(raw)
	case "function_call_output", "custom_tool_call_output":
		if out, images, ok := respToolOutput(m); ok {
			r.changed = true
			r.emitToolOutput(out, images)
		} else {
			r.emitToolOutput(raw, nil)
		}
	case "reasoning":
		r.emitQuiet(raw)
	case "additional_tools":
		if out, ok := respTools(m["tools"], r.tools); ok {
			m["tools"] = out
			raw = marshal(m)
			r.changed = true
		}
		r.emitQuiet(raw)
	case "compaction", "compaction_summary":
		r.changed, r.compacted = true, true
	case "item_reference":
		r.changed = true // a pointer into server-side storage this upstream does not have
	default:
		role, text := respItemText(t, m)
		r.emitText(role, text)
	}
}

// respContent rewrites the parts of a message. User parts stay separate
// upstream; the other roles are joined into one text, so rewritten parts
// get separators there.
func respContent(raw json.RawMessage, role string) (json.RawMessage, bool) {
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil, false // the translator reports unknown roles
	}
	parts, ok := array(raw)
	if !ok {
		return nil, false
	}
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	system := role == "system" || role == "developer"
	pieces := make([]piece, 0, len(parts))
	changed := false
	for _, p := range parts {
		t, isObj := kindOf(p)
		pm, _ := object(p)
		switch {
		case !isObj:
			pieces = append(pieces, keep(p))
		case t == "input_text" || t == "output_text" || t == "text":
			pieces = append(pieces, origText(p))
		case (t == "input_image" || t == "image_url") && (!system && partImageURL(pm) != "" || imageMalformed(pm)):
			pieces = append(pieces, keep(p)) // renderable, or for the translator to report
		case t == "input_image" || t == "image_url":
			pieces = append(pieces, synthText(respImageMarker(pm)))
			changed = true
		default:
			pieces = append(pieces, synthText(respPartText(t, pm)))
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	return assemble(pieces, role != "user", textType), true
}

// respToolOutput rewrites a function_call_output whose output holds parts
// the translator cannot take: text parts and rewritten parts become one
// string, and images are returned as user-message parts labelled with the
// call id, mirroring the Claude tool_result path.
func respToolOutput(m map[string]json.RawMessage) (json.RawMessage, []json.RawMessage, bool) {
	parts, ok := array(m["output"])
	if !ok {
		return nil, nil, false
	}
	var pieces []piece
	var urls []string
	changed := false
	for _, p := range parts {
		t, isObj := kindOf(p)
		pm, _ := object(p)
		switch {
		case !isObj:
			return nil, nil, false // the translator reports malformed output
		case t == "input_text" || t == "output_text" || t == "text":
			pieces = append(pieces, origText(p))
		case (t == "input_image" || t == "image_url") && partImageURL(pm) != "":
			urls = append(urls, partImageURL(pm))
			changed = true
		case (t == "input_image" || t == "image_url") && imageMalformed(pm):
			return nil, nil, false // the translator reports it
		case t == "input_image" || t == "image_url":
			pieces = append(pieces, synthText(respImageMarker(pm)))
			changed = true
		default:
			pieces = append(pieces, synthText(respPartText(t, pm)))
			changed = true
		}
	}
	if !changed {
		return nil, nil, false
	}
	var b strings.Builder
	wrote, prevSynth := false, false
	for _, p := range pieces {
		text := p.value()
		if wrote && text != "" && (p.synth || prevSynth) {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
		if text != "" {
			wrote, prevSynth = true, p.synth
		}
	}
	callID := str(m, "call_id")
	var images []json.RawMessage
	if len(urls) > 0 {
		note := "[Images from this tool result are in the following user message, labeled with tool_call_id " + callID + ".]"
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(note)
		images = append(images, marshal(map[string]string{"type": "input_text", "text": "Tool result images for tool_call_id " + callID + ":"}))
		for _, u := range urls {
			images = append(images, marshal(map[string]string{"type": "input_image", "image_url": u}))
		}
	}
	m["output"] = marshal(b.String())
	return marshal(m), images, true
}

// partImageURL returns the URL of an image part ("image_url" as a string
// or {"url": ...}), or "" when the part only references an uploaded file.
func partImageURL(m map[string]json.RawMessage) string {
	if u := str(m, "image_url"); u != "" {
		return u
	}
	obj, _ := object(m["image_url"])
	return str(obj, "url")
}

// imageMalformed reports an image part with neither a URL nor a file
// reference: malformed input rather than an unsupported source.
func imageMalformed(m map[string]json.RawMessage) bool {
	return partImageURL(m) == "" && str(m, "file_id") == ""
}

func respImageMarker(m map[string]json.RawMessage) string {
	if id := str(m, "file_id"); id != "" && partImageURL(m) == "" {
		return "[image omitted: uploaded file " + id + " is not available here]"
	}
	return "[image omitted]"
}

// respPartText renders a message or tool-output part other than text and
// images.
func respPartText(t string, m map[string]json.RawMessage) string {
	switch t {
	case "input_file":
		return responsesFile(m).render()
	case "refusal":
		return "[Refusal] " + str(m, "refusal")
	}
	return omitted(t)
}

// respItemText renders an input item the translator has no mapping for,
// with the role its message takes: what the model did is assistant text,
// what came back to it is user text.
func respItemText(t string, m map[string]json.RawMessage) (role, text string) {
	switch t {
	case "web_search_call":
		return "assistant", webSearchCallText(m)
	case "local_shell_call":
		action, _ := object(m["action"])
		var cmd []string
		json.Unmarshal(action["command"], &cmd)
		text := "[Local shell command: " + strings.Join(cmd, " ") + "]"
		if wd := str(action, "working_directory"); wd != "" {
			text += " (in " + wd + ")"
		}
		return "assistant", text
	case "local_shell_call_output":
		return "user", "[Local shell output]\n" + outputString(m["output"])
	case "shell_call":
		action, _ := object(m["action"])
		var cmds []string
		json.Unmarshal(action["commands"], &cmds)
		return "assistant", "[Shell command]\n$ " + strings.Join(cmds, "\n$ ")
	case "shell_call_output":
		return "user", shellOutputText(m["output"])
	case "apply_patch_call":
		op, _ := object(m["operation"])
		text := "[apply_patch: " + joinNonEmpty(" ", str(op, "type"), str(op, "path")) + "]"
		if diff := str(op, "diff"); diff != "" {
			text += "\n" + diff
		}
		return "assistant", text
	case "apply_patch_call_output":
		text := "[apply_patch result: " + str(m, "status") + "]"
		if out := outputString(m["output"]); out != "" {
			text += "\n" + out
		}
		return "user", text
	case "computer_call":
		return "assistant", "[Computer action: " + compact(m["action"]) + "]"
	case "computer_call_output":
		return "user", "[Computer screenshot omitted]"
	case "image_generation_call":
		if p := str(m, "revised_prompt"); p != "" {
			return "assistant", "[Generated an image: " + p + "]"
		}
		return "assistant", "[Generated an image]"
	case "code_interpreter_call":
		return "assistant", codeInterpreterText(m)
	case "file_search_call":
		return "assistant", fileSearchText(m)
	case "mcp_list_tools":
		tools, _ := array(m["tools"])
		var names []string
		for _, tl := range tools {
			if tm, ok := object(tl); ok && str(tm, "name") != "" {
				names = append(names, str(tm, "name"))
			}
		}
		return "assistant", "[MCP server " + str(m, "server_label") + " tools: " + strings.Join(names, ", ") + "]"
	case "mcp_approval_request":
		return "assistant", "[MCP approval requested: " + joinNonEmpty("/", str(m, "server_label"), str(m, "name")) + " " + str(m, "arguments") + "]"
	case "mcp_approval_response":
		var approve bool
		json.Unmarshal(m["approve"], &approve)
		if approve {
			return "user", "[MCP approval: approved]"
		}
		return "user", "[MCP approval: denied]"
	case "mcp_call":
		text := "[MCP tool call: " + joinNonEmpty("/", str(m, "server_label"), str(m, "name")) + " " + str(m, "arguments") + "]"
		if e := outputString(m["error"]); e != "" {
			text += "\nError: " + e
		} else if out := outputString(m["output"]); out != "" {
			text += "\n" + out
		}
		return "assistant", text
	}
	role = "assistant"
	if strings.HasSuffix(t, "_output") {
		role = "user"
	}
	if t == "" {
		return role, "[untyped item omitted]"
	}
	return role, "[" + t + " item omitted]"
}

// outputString renders an output field that may be a string or any other
// JSON value.
func outputString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return compact(raw)
}

func webSearchCallText(m map[string]json.RawMessage) string {
	action, _ := object(m["action"])
	switch str(action, "type") {
	case "search":
		query := str(action, "query")
		if query == "" {
			var qs []string
			json.Unmarshal(action["queries"], &qs)
			query = strings.Join(qs, "; ")
		}
		text := "[Web search: " + query + "]"
		sources, _ := array(action["sources"])
		var urls []string
		for _, s := range sources {
			if sm, ok := object(s); ok && str(sm, "url") != "" {
				urls = append(urls, str(sm, "url"))
			}
		}
		if len(urls) > 0 {
			text += "\nSources:\n- " + strings.Join(urls, "\n- ")
		}
		return text
	case "open_page":
		return "[Opened web page: " + str(action, "url") + "]"
	case "find", "find_in_page":
		return "[Searched " + str(action, "url") + " for: " + str(action, "pattern") + "]"
	}
	return "[Web search]"
}

func shellOutputText(raw json.RawMessage) string {
	chunks, ok := array(raw)
	if !ok {
		if s := outputString(raw); s != "" {
			return "[Shell output]\n" + s
		}
		return "[Shell output]"
	}
	var b strings.Builder
	b.WriteString("[Shell output]")
	for _, c := range chunks {
		cm, ok := object(c)
		if !ok {
			continue
		}
		if s := str(cm, "stdout"); s != "" {
			b.WriteString("\n" + s)
		}
		if s := str(cm, "stderr"); s != "" {
			b.WriteString("\nstderr:\n" + s)
		}
		outcome, _ := object(cm["outcome"])
		switch str(outcome, "type") {
		case "exit":
			b.WriteString("\n(exit code " + compact(outcome["exit_code"]) + ")")
		case "timeout":
			b.WriteString("\n(timed out)")
		}
	}
	return b.String()
}

func codeInterpreterText(m map[string]json.RawMessage) string {
	var b strings.Builder
	b.WriteString("[Code interpreter]")
	if code := str(m, "code"); code != "" {
		b.WriteString("\n" + code)
	}
	outputs, _ := array(m["outputs"])
	for _, o := range outputs {
		om, ok := object(o)
		if !ok {
			continue
		}
		switch str(om, "type") {
		case "logs":
			b.WriteString("\nOutput:\n" + str(om, "logs"))
		case "image":
			b.WriteString("\n[output image omitted]")
		}
	}
	return b.String()
}

func fileSearchText(m map[string]json.RawMessage) string {
	var queries []string
	json.Unmarshal(m["queries"], &queries)
	var b strings.Builder
	b.WriteString("[File search: " + strings.Join(queries, "; ") + "]")
	results, _ := array(m["results"])
	for i, r := range results {
		rm, ok := object(r)
		if !ok {
			continue
		}
		b.WriteString("\n" + strconv.Itoa(i+1) + ". " + joinNonEmpty(": ", str(rm, "filename"), str(rm, "text")))
	}
	return b.String()
}
