package sanitize

import (
	"encoding/json"
	"unicode/utf8"
)

// A minimal JSON splitter for the read side of this package. Most requests
// need no rewrite, and deciding that with encoding/json meant decoding and
// copying every level of a multi-megabyte history several times. These
// helpers return sub-slices of the input instead and do not validate:
// anything this package rewrites is re-encoded with encoding/json, which
// validates every value it copies, and a body that fails there is returned
// unchanged for the translator to report.

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func skipSpace(raw []byte, i int) int {
	for i < len(raw) && isSpace(raw[i]) {
		i++
	}
	return i
}

// skipString returns the index just past the string starting at raw[i]
// (which must be '"'), or -1 when it is not terminated.
func skipString(raw []byte, i int) int {
	for i++; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}

// skipValue returns the index just past the value starting at raw[i], or
// -1 when there is no complete value there.
func skipValue(raw []byte, i int) int {
	if i >= len(raw) {
		return -1
	}
	switch raw[i] {
	case '"':
		return skipString(raw, i)
	case '{', '[':
		depth := 0
		for ; i < len(raw); i++ {
			switch raw[i] {
			case '"':
				if i = skipString(raw, i); i < 0 {
					return -1
				}
				i--
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		return -1
	case ',', ':', '}', ']':
		return -1
	}
	start := i
	for i < len(raw) && !isSpace(raw[i]) && raw[i] != ',' && raw[i] != '}' && raw[i] != ']' {
		i++
	}
	if i == start {
		return -1
	}
	return i
}

// splitObject calls fn for each member of the JSON object raw, in order.
// It reports false when raw is not a single object.
func splitObject(raw []byte, fn func(key, val []byte)) bool {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return false
	}
	i = skipSpace(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return skipSpace(raw, i+1) == len(raw)
	}
	for {
		if i >= len(raw) || raw[i] != '"' {
			return false
		}
		k := skipString(raw, i)
		if k < 0 {
			return false
		}
		key := raw[i:k]
		i = skipSpace(raw, k)
		if i >= len(raw) || raw[i] != ':' {
			return false
		}
		i = skipSpace(raw, i+1)
		v := skipValue(raw, i)
		if v < 0 {
			return false
		}
		fn(key, raw[i:v])
		i = skipSpace(raw, v)
		if i >= len(raw) {
			return false
		}
		switch raw[i] {
		case ',':
			i = skipSpace(raw, i+1)
		case '}':
			return skipSpace(raw, i+1) == len(raw)
		default:
			return false
		}
	}
}

// splitArray returns the elements of the JSON array raw, or false when raw
// is not a single array.
func splitArray(raw []byte) ([]json.RawMessage, bool) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return nil, false
	}
	out := []json.RawMessage{}
	i = skipSpace(raw, i+1)
	if i < len(raw) && raw[i] == ']' {
		return out, skipSpace(raw, i+1) == len(raw)
	}
	for {
		v := skipValue(raw, i)
		if v < 0 {
			return nil, false
		}
		out = append(out, raw[i:v:v])
		i = skipSpace(raw, v)
		if i >= len(raw) {
			return nil, false
		}
		switch raw[i] {
		case ',':
			i = skipSpace(raw, i+1)
		case ']':
			return out, skipSpace(raw, i+1) == len(raw)
		default:
			return nil, false
		}
	}
}

// decodeString decodes a JSON string value; ok is false for any other
// value. Plain strings (no escapes or control bytes, valid UTF-8) are
// converted directly; the rest go through encoding/json.
func decodeString(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	body := raw[1 : len(raw)-1]
	plain := true
	for _, c := range body {
		if c == '\\' || c == '"' || c < 0x20 {
			plain = false
			break
		}
	}
	if plain && utf8.Valid(body) {
		return string(body), true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// keyIs reports whether the raw (quoted) key decodes to name.
func keyIs(key []byte, name string) bool {
	if len(key) == len(name)+2 && string(key[1:len(key)-1]) == name {
		return true
	}
	if k, ok := decodeString(key); ok {
		return k == name
	}
	return false
}
