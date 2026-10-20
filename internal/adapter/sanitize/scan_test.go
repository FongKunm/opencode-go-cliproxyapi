package sanitize

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

var scanCorpus = []string{
	`{}`, `[]`, ` { } `, "\t[\n]\r\n",
	`{"a":1,"b":"x","c":[1,2,{"d":null}],"e":{"f":true,"g":false},"h":-1.5e3}`,
	`[1,"two",[3],{"four":4},null,true,false,0.5,-0,1E9]`,
	`{"s":"quote \" backslash \\ slash \/ tab \t nl \n uni 中文 surrogate 😀"}`,
	`{"brackets":"[{]}","nested":[[[[{"x":"}]"}]]]],"empty":""}`,
	`{"dup":1,"dup":2,"Dup":3}`,
	`{"k\"q":1,"k\\":2,"type":"text"}`,
	`[ "a" , { "b" : [ ] } , 3 ]`,
	`{"utf8":"中文 ✓ é","bad":"é"}`,
	`{"deep":` + string(bytes.Repeat([]byte(`[`), 50)) + string(bytes.Repeat([]byte(`]`), 50)) + `}`,
}

func TestSplitMatchesEncodingJSON(t *testing.T) {
	for _, in := range scanCorpus {
		checkAgainstEncodingJSON(t, []byte(in))
	}
}

// The splitter does not validate (re-encoding does, see
// FuzzSanitizeNeverBreaksJSON), but it must reject what is not a single
// object or array at its level.
func TestSplitRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		``, `{`, `[`, `null`, `"s"`, `{"a"}`, `{"a":}`, `{"a":1,}`, `{"a":1,"b"}`, `{"a":"unterminated}`,
		`[1 2]`, `{"a":1}{"b":2}`, `{"a":1} x`, `{a:1}`,
	} {
		if _, ok := object(json.RawMessage(in)); ok {
			t.Fatalf("object(%q) accepted", in)
		}
	}
	for _, in := range []string{``, `[`, `{}`, `[1,]`, `[1 2]`, `[1]]`, `["x",`, `[1] x`, `["unterminated]`} {
		if _, ok := array(json.RawMessage(in)); ok {
			t.Fatalf("array(%q) accepted", in)
		}
	}
}

// A malformed value the splitter does not notice must not leak into the
// output: re-encoding rejects it and the original body is returned.
func TestRewriteOfMalformedBodyReturnsOriginal(t *testing.T) {
	for _, c := range []struct {
		fn   func([]byte) []byte
		body string
	}{
		{ClaudeMessages, `{"messages":[{"role":"user","content":[{"type":"video"}]}],"x":[1},"y":2}`},
		{ClaudeMessages, `{"messages":[{"role":"user","content":[{"type":"video"},{"type":"text","text":"a","z":[}]}]}`},
		{Responses, `{"input":[{"type":"web_search_call"},{"type":"message","role":"user","content":"a","z":{]}}]}`},
	} {
		in := []byte(c.body)
		if out := c.fn(in); !bytes.Equal(out, in) {
			t.Fatalf("%s rewritten to %s", in, out)
		}
	}
}

// checkAgainstEncodingJSON asserts that for valid JSON the splitter sees
// exactly what encoding/json sees.
func checkAgainstEncodingJSON(t *testing.T, in []byte) {
	t.Helper()
	if !json.Valid(in) {
		return
	}
	var want map[string]json.RawMessage
	if json.Unmarshal(in, &want) == nil && want != nil {
		got, ok := object(in)
		if !ok {
			t.Fatalf("object(%q) failed", in)
		}
		if len(got) != len(want) {
			t.Fatalf("object(%q) keys = %d, want %d", in, len(got), len(want))
		}
		for k, w := range want {
			if !jsonEqual(got[k], w) {
				t.Fatalf("object(%q)[%q] = %s, want %s", in, k, got[k], w)
			}
		}
	}
	var wantArr []json.RawMessage
	if json.Unmarshal(in, &wantArr) == nil && wantArr != nil {
		got, ok := array(in)
		if !ok || len(got) != len(wantArr) {
			t.Fatalf("array(%q) = %d elements, %v; want %d", in, len(got), ok, len(wantArr))
		}
		for i := range got {
			if !jsonEqual(got[i], wantArr[i]) {
				t.Fatalf("array(%q)[%d] = %s, want %s", in, i, got[i], wantArr[i])
			}
		}
	}
	var wantStr string
	if json.Unmarshal(in, &wantStr) == nil && bytes.HasPrefix(bytes.TrimSpace(in), []byte(`"`)) {
		if got, ok := decodeString(bytes.TrimSpace(in)); !ok || got != wantStr {
			t.Fatalf("decodeString(%q) = %q, %v; want %q", in, got, ok, wantStr)
		}
	}
	var typed struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(in, &typed) == nil && bytes.HasPrefix(bytes.TrimSpace(in), []byte("{")) {
		if got := typeOf(in); got != typed.Type {
			t.Fatalf("typeOf(%q) = %q, want %q", in, got, typed.Type)
		}
	}
}

func jsonEqual(a, b []byte) bool {
	if bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b)) {
		return true
	}
	decode := func(raw []byte) (any, bool) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // numbers beyond float64 range stay comparable
		var v any
		return v, dec.Decode(&v) == nil
	}
	x, okX := decode(a)
	y, okY := decode(b)
	return okX && okY && reflect.DeepEqual(x, y)
}

func FuzzSplitAgreesWithEncodingJSON(f *testing.F) {
	for _, in := range scanCorpus {
		f.Add([]byte(in))
	}
	f.Add([]byte(`{"type":"document","Type":"text"}`))
	f.Add([]byte(`{"TYPE":"x"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		checkAgainstEncodingJSON(t, in)
		// Never panics on anything.
		object(in)
		array(in)
		typeOf(in)
		decodeString(in)
	})
}

// FuzzSanitizeNeverBreaksJSON: whatever the input, the rewrite returns the
// body itself or valid JSON, and never panics.
func FuzzSanitizeNeverBreaksJSON(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"video"},{"type":"text","text":"a"}]}]}`))
	f.Add([]byte(`{"system":[{"type":"doc"}],"tools":[{"type":"web_search_20250305","name":"w"}],"tool_choice":{"type":"tool","name":"w"}}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"search_result","source":"s"}]}]}]}`))
	f.Add([]byte(`{"input":[{"type":"web_search_call","action":{"type":"search","query":"q"}},{"type":"compaction"}],"tools":[{"type":"local_shell"}]}`))
	f.Add([]byte(`{"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"x"}]}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"video"}]}],"x":[1}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"video"}]}],"x":[1},"y":2}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, fn := range []func([]byte) []byte{ClaudeMessages, Responses} {
			out := fn(in)
			if !bytes.Equal(out, in) && !json.Valid(out) {
				t.Fatalf("invalid output %q for input %q", out, in)
			}
		}
	})
}
