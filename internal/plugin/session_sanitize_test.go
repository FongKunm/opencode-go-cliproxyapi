package plugin

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Without a session header or CPA canonical id the executor derives the
// OpenCode session from the request content before translating it. Content
// the Chat Completions route rewrites (internal/adapter/sanitize) must not
// fail that derivation first.
func TestSessionDerivationToleratesRewrittenContent(t *testing.T) {
	doc := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 not really"))
	cases := []struct{ name, format, body string }{
		{"claude document in system", "claude",
			`{"model":"opencode-go/glm-5.2","max_tokens":64,"system":[{"type":"text","text":"s"},{"type":"document","source":{"type":"text","data":"policy"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`},
		{"claude search_result in first turn", "claude",
			`{"model":"opencode-go/glm-5.2","max_tokens":64,"messages":[{"role":"user","content":[` +
				`{"type":"search_result","source":"https://kb/x","title":"T","content":[{"type":"text","text":"c"}]},{"type":"text","text":"use it"}]}]}`},
		{"claude document in first-turn tool_result", "claude",
			`{"model":"opencode-go/glm-5.2","max_tokens":64,"messages":[{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"t","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + doc + `"}}]}]}]}`},
		{"claude file image later in history", "claude",
			`{"model":"opencode-go/glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yes"},` +
				`{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"file_1"}}]}]}`},
		{"responses input_file in first message", "openai-response",
			`{"model":"opencode-go/glm-5.2","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read"},` +
				`{"type":"input_file","filename":"a.txt","file_data":"data:text/plain;base64,aGVsbG8="}]}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _, st, _ := newIntegrationManager(t)
			env := mustExecute(t, m, "opencode-go/glm-5.2", c.format, []byte(c.body))
			if !env.OK {
				t.Fatalf("execute failed: %+v", env.Error)
			}
			st.mu.Lock()
			session, upstream := st.lastChatSession, string(st.lastChatBody)
			st.mu.Unlock()
			if session == "" || session == emptyOpenCodeSessionID {
				t.Fatalf("x-opencode-session = %q, want a content-derived id", session)
			}
			if strings.Contains(upstream, `"type":"document"`) || strings.Contains(upstream, `"input_file"`) || strings.Contains(upstream, `"search_result"`) {
				t.Fatalf("unsupported content reached upstream: %s", upstream)
			}
		})
	}
}

// Bodies that derive cleanly keep the exact session id they had before.
func TestSessionDerivationUnchangedForCleanBodies(t *testing.T) {
	for _, c := range []struct{ format, body string }{
		{"claude", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"document","source":{"type":"text","data":"d"}}]}]}`},
		{"openai-response", `{"input":[{"type":"message","role":"user","content":"hi"}]}`},
	} {
		want, eErr := deriveOpenCodeSessionID(c.format, []byte(c.body))
		if eErr != nil {
			t.Fatalf("%s: derive: %v", c.format, eErr)
		}
		got, eErr := resolveOpenCodeSessionID(executorRequestFor(c.format, c.body))
		if eErr != nil || got != want {
			t.Fatalf("%s: resolve = %q, %v; want %q", c.format, got, eErr, want)
		}
	}
}

func executorRequestFor(format, body string) executorRequest {
	var req executorRequest
	req.SourceFormat = format
	req.OriginalRequest = []byte(body)
	return req
}
