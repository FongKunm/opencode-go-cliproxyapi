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

// object splits raw as a JSON object; values are sub-slices of raw and a
// repeated key keeps its last value, as with encoding/json.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	m := make(map[string]json.RawMessage)
	ok := splitObject(raw, func(key, val []byte) {
		k, isStr := decodeString(key)
		if !isStr {
			k = string(key)
		}
		m[k] = val[:len(val):len(val)]
	})
	if !ok {
		return nil, false
	}
	return m, true
}

// array splits raw as a JSON array; elements are sub-slices of raw.
func array(raw json.RawMessage) ([]json.RawMessage, bool) {
	return splitArray(raw)
}

// field returns member name of the JSON object raw the way the
// translator's struct decoding finds it: keys match case-insensitively and
// the last match wins.
func field(raw json.RawMessage, name string) (json.RawMessage, bool) {
	var val json.RawMessage
	found := false
	splitObject(raw, func(key, v []byte) {
		if keyIs(key, name) || foldKeyIs(key, name) {
			val, found = v, true
		}
	})
	return val, found
}

func foldKeyIs(key []byte, name string) bool {
	k, ok := decodeString(key)
	return ok && strings.EqualFold(k, name)
}

// typeOf returns the "type" member of a JSON object, or "" when raw is not
// an object or has no string type.
func typeOf(raw json.RawMessage) string {
	v, _ := field(raw, "type")
	s, _ := decodeString(v)
	return s
}

// str returns the string member key of m, or "".
func str(m map[string]json.RawMessage, key string) string {
	s, _ := decodeString(m[key])
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

// encodeOr re-encodes a rewritten request, or returns fallback when that
// fails: values split out of malformed input do not survive re-encoding,
// and the translator then reports the original body as before.
func encodeOr(v any, fallback []byte) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return fallback
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// marshal encodes values this package composes. A value carrying an
// invalid raw element encodes to invalid JSON, which encodeOr rejects.
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
// kept verbatim, or text — original or synthesized — that may need a
// separator from its neighbours.
type piece struct {
	raw     json.RawMessage // element to emit verbatim
	text    string          // text, when hasText
	hasText bool            // text is set (otherwise read from raw on demand)
	isText  bool            // the element counts as text where text is joined
	synth   bool            // produced by this package
}

func keep(raw json.RawMessage) piece { return piece{raw: raw} }

func synthText(s string) piece { return piece{text: s, hasText: true, isText: true, synth: true} }

// origText wraps an original text element. Its text is only decoded if it
// has to be re-emitted with a separator.
func origText(raw json.RawMessage) piece { return piece{raw: raw, isText: true} }

// value returns the text of a text piece.
func (p piece) value() string {
	if p.hasText {
		return p.text
	}
	v, _ := field(p.raw, "text")
	s, _ := decodeString(v)
	return s
}

// nonEmpty reports whether a text piece has text; original elements are
// assumed to (decoding them just to check would cost more than an extra
// separator in the rare case they are empty).
func (p piece) nonEmpty() bool { return !p.hasText || p.text != "" }

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
		case sep && wrote && p.nonEmpty() && (p.synth || prevSynth):
			out = append(out, textElem(textType, "\n\n"+p.value()))
		case p.synth:
			out = append(out, textElem(textType, p.text))
		default:
			out = append(out, p.raw)
		}
		if p.nonEmpty() {
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
