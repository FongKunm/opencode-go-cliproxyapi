package chatcompletions

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// buildEffort translates body for upstreamModel and returns the outbound
// reasoning_effort and whether the field is present at all.
func buildEffort(t *testing.T, upstreamModel, sourceFormat, body string, ts *pluginapi.ThinkingSupport) (any, bool) {
	t.Helper()
	out, eErr := BuildRequest(upstreamModel, sourceFormat, []byte(body), ts)
	m := decodeOut(t, out, eErr)
	v, ok := m["reasoning_effort"]
	return v, ok
}

// TestClaudeAdaptiveEffortReachesUpstream pins the Claude Code adaptive
// thinking shape ({"type":"adaptive"} + output_config.effort): the effort is
// forwarded as reasoning_effort, with DeepSeek taking the full ladder and
// other families clamped to their declared (or default) levels.
func TestClaudeAdaptiveEffortReachesUpstream(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		effort string
		ts     *pluginapi.ThinkingSupport
		want   string
	}{
		{"deepseek max", "deepseek-v4-pro", "max", nil, "max"},
		{"deepseek xhigh", "deepseek-v4.1-flash", "xhigh", nil, "xhigh"},
		{"deepseek low", "deepseek-v4-pro", "low", nil, "low"},
		{"deepseek uppercase", "deepseek-v4-pro", "MAX", nil, "max"},
		{"deepseek ignores a narrower catalog ladder", "deepseek-v4-pro", "max",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "max"},
		{"other family default ladder caps max", "kimi-k2", "max", nil, "high"},
		{"other family declared ladder kept", "glm-5.3", "max",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}, "xhigh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"` + c.effort + `"},"messages":[{"role":"user","content":"hi"}]}`
			got, ok := buildEffort(t, c.model, "claude", body, c.ts)
			if !ok || got != c.want {
				t.Fatalf("reasoning_effort = %v (present %v), want %q", got, ok, c.want)
			}
		})
	}
}

func TestClaudeThinkingControlMatrix(t *testing.T) {
	cases := []struct {
		name  string
		model string
		body  string
		want  string // "" means the field must be absent
	}{
		{"adaptive without effort omits", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"adaptive"}}`, ""},
		{"no thinking control omits", "deepseek-v4-pro",
			`{"max_tokens":64}`, ""},
		{"effort without thinking control", "deepseek-v4-pro",
			`{"max_tokens":64,"output_config":{"effort":"medium"}}`, "medium"},
		{"disabled is none on deepseek", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"disabled"}}`, "none"},
		{"disabled outranks effort", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`, "none"},
		{"disabled clamps to weakest elsewhere", "kimi-k2",
			`{"max_tokens":64,"thinking":{"type":"disabled"}}`, "low"},
		{"enabled budget mapping unchanged", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":8192}}`, "medium"},
		{"enabled large budget reaches xhigh on deepseek", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":31999}}`, "xhigh"},
		{"enabled effort outranks budget", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"max"}}`, "max"},
		{"unknown effort ignored", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"ultra"}}`, ""},
		{"non-string effort tolerated", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":3}}`, ""},
		{"non-object output_config tolerated", "deepseek-v4-pro",
			`{"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":"max"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := buildEffort(t, c.model, "claude", c.body, nil)
			if c.want == "" {
				if ok {
					t.Fatalf("reasoning_effort must be omitted, got %v", got)
				}
				return
			}
			if !ok || got != c.want {
				t.Fatalf("reasoning_effort = %v (present %v), want %q", got, ok, c.want)
			}
		})
	}
}

// TestResponsesAndChatEffortsUntouchedOnDeepSeek pins that the DeepSeek
// ladder changes nothing for the Responses and Chat inbound legs: both
// forward the client's effort verbatim (no clamping existed there before).
func TestResponsesAndChatEffortsUntouchedOnDeepSeek(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "xhigh", "max"} {
		got, ok := buildEffort(t, "deepseek-v4-pro", "openai-response",
			`{"reasoning":{"effort":"`+effort+`"},"input":"hi"}`, nil)
		if !ok || got != effort {
			t.Fatalf("responses effort %q forwarded as %v (present %v)", effort, got, ok)
		}
		got, ok = buildEffort(t, "deepseek-v4-pro", "openai",
			`{"model":"x","reasoning_effort":"`+effort+`","messages":[{"role":"user","content":"hi"}]}`, nil)
		if !ok || got != effort {
			t.Fatalf("chat effort %q forwarded as %v (present %v)", effort, got, ok)
		}
	}
	if got, ok := buildEffort(t, "deepseek-v4-pro", "openai-response", `{"input":"hi"}`, nil); ok {
		t.Fatalf("responses without effort must omit the field, got %v", got)
	}
}
