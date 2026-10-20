// Package sanitize rewrites an inbound request body, before the Chat
// Completions translator sees it, so that conversation history the
// upstream cannot represent never fails the request.
//
// Agent clients replay the whole history on every turn. A single block the
// translator rejects — a PDF the client read as a document, a server-side
// web search from an earlier turn on real Claude, a compaction item left by
// a GPT model — used to turn every later turn into the same 400, so the
// conversation was stuck for good. This package keeps what it can as text
// (search results, tool output, document text), replaces the rest with a
// one-line English marker, removes hosted tool definitions the upstream
// cannot run, and leaves everything else byte-for-byte alone: a body with
// nothing to rewrite is returned as the same slice.
//
// Only content that is valid but unsupported is rewritten. Structurally
// malformed input (a block that is not an object, an image without data)
// passes through so the translator keeps reporting it.
package sanitize

import (
	"bytes"
	"encoding/json"
	"strings"
)

// object decodes raw as a JSON object.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// array decodes raw as a JSON array.
func array(raw json.RawMessage) ([]json.RawMessage, bool) {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil || a == nil {
		return nil, false
	}
	return a, true
}

// typeOf returns the "type" member of a JSON object, or "" when raw is not
// an object or has no string type.
func typeOf(raw json.RawMessage) string {
	var v struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.Type
}

// str returns the string member key of m, or "".
func str(m map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := m[key]; ok {
		json.Unmarshal(raw, &s)
	}
	return s
}

// compact renders a JSON value on one line for markers ("{}" when empty).
func compact(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var b strings.Builder
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSpace(b.String())
}

// marshal encodes values this package composes; they always encode.
func marshal(v any) json.RawMessage {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return json.RawMessage(strings.TrimSuffix(b.String(), "\n"))
}

// omitted is the marker for content that has no text form.
func omitted(kind string) string {
	if kind == "" {
		return "[untyped content omitted]"
	}
	return "[" + kind + " content omitted]"
}

// piece is one element of a rewritten content array: an original element
// kept verbatim, or text — original or synthesized — that may be merged
// with its neighbours.
type piece struct {
	raw    json.RawMessage // element to emit verbatim when not merged
	text   string          // text of a text piece
	isText bool
	synth  bool // produced by this package
}

func keep(raw json.RawMessage) piece { return piece{raw: raw} }

func synthText(s string) piece { return piece{text: s, isText: true, synth: true} }

// origText wraps an original text element, keeping its bytes for when it
// is not merged.
func origText(raw json.RawMessage) piece {
	m, _ := object(raw)
	return piece{raw: raw, text: str(m, "text"), isText: true}
}

// assemble renders pieces into a content array; textType names the element
// type of synthesized text ("text", "input_text", "output_text").
//
// With sep set, the target joins the text elements of the array without a
// separator (skipping non-text elements), so synthesized text is set off
// from the text before and after it by a blank line. Original text that
// needs no separator keeps its exact bytes.
func assemble(pieces []piece, sep bool, textType string) json.RawMessage {
	out := make([]json.RawMessage, 0, len(pieces))
	wrote, prevSynth := false, false
	for _, p := range pieces {
		if !p.isText {
			out = append(out, p.raw)
			continue
		}
		switch {
		case sep && wrote && p.text != "" && (p.synth || prevSynth):
			out = append(out, textElem(textType, "\n\n"+p.text))
		case p.synth:
			out = append(out, textElem(textType, p.text))
		default:
			out = append(out, p.raw)
		}
		if p.text != "" {
			wrote, prevSynth = true, p.synth
		}
	}
	return marshal(out)
}

func textElem(textType, text string) json.RawMessage {
	return marshal(map[string]string{"type": textType, "text": text})
}

// kindOf returns the type of a JSON object element; isObj is false for any
// other JSON value, which callers leave for the translator to report.
func kindOf(raw json.RawMessage) (t string, isObj bool) {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	return typeOf(raw), true
}
