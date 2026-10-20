package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const suffixCatalog = `{"data":[
	{"id":"deepseek-v4-pro"},
	{"id":"glm-5.3"},
	{"id":"glm-x(beta)"},
	{"id":"minimax-m3"}
]}`

func newSuffixManager(t *testing.T) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(suffixCatalog, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
		"/v1/messages":         claudeResponseBody,
	}))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

// upstreamJSON executes one non-stream request and returns the upstream
// request body the plugin sent.
func upstreamJSON(t *testing.T, m *Manager, f *fakeCaller, model, format, body string) map[string]any {
	t.Helper()
	env := mustExecute(t, m, model, format, []byte(body))
	if !env.OK {
		t.Fatalf("execute %s: %+v", model, env.Error)
	}
	var up map[string]any
	if err := json.Unmarshal(wireBody(t, lastWire(t, f, pluginabi.MethodHostHTTPDo), "body"), &up); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	return up
}

const (
	suffixClaudeAdaptiveMax = `{"model":"x","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`
	suffixClaudePlain       = `{"model":"x","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	suffixChatWithEffort    = `{"model":"x","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`
	suffixResponsesHigh     = `{"model":"x","reasoning":{"effort":"high"},"input":"hi"}`
)

func TestSuffixModelRoutesAndForcesEffort(t *testing.T) {
	m, f := newSuffixManager(t)
	cases := []struct {
		name, model, format, body string
		want                      string // "" = field must be absent
	}{
		{"level on plain request", "opencode-go/deepseek-v4-pro(high)", "claude", suffixClaudePlain, "high"},
		{"bare upstream id with suffix", "deepseek-v4-pro(xhigh)", "claude", suffixClaudePlain, "xhigh"},
		{"suffix outranks output_config.effort", "deepseek-v4-pro(low)", "claude", suffixClaudeAdaptiveMax, "low"},
		{"suffix outranks chat reasoning_effort", "deepseek-v4-pro(MAX)", "openai", suffixChatWithEffort, "max"},
		{"suffix outranks responses reasoning.effort", "deepseek-v4-pro(minimal)", "openai-response", suffixResponsesHigh, "minimal"},
		{"none on deepseek", "deepseek-v4-pro(none)", "claude", suffixClaudeAdaptiveMax, "none"},
		{"budget suffix", "deepseek-v4-pro(8192)", "claude", suffixClaudePlain, "medium"},
		{"auto removes the field", "deepseek-v4-pro(auto)", "openai", suffixChatWithEffort, ""},
		{"auto removes a translated effort", "deepseek-v4-pro(-1)", "claude", suffixClaudeAdaptiveMax, ""},
		{"other family clamps to its ladder", "glm-5.3(max)", "openai", suffixChatWithEffort, "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := upstreamJSON(t, m, f, c.model, c.format, c.body)
			base, _, _ := strings.Cut(strings.TrimPrefix(c.model, "opencode-go/"), "(")
			if up["model"] != base {
				t.Fatalf("upstream model = %v, want the bare id %q", up["model"], base)
			}
			got, present := up["reasoning_effort"]
			if c.want == "" {
				if present {
					t.Fatalf("reasoning_effort must be removed, got %v", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("reasoning_effort = %v (present %v), want %q", got, present, c.want)
			}
		})
	}
}

func TestSuffixUnknownValueRoutesWithoutOverride(t *testing.T) {
	m, f := newSuffixManager(t)
	up := upstreamJSON(t, m, f, "deepseek-v4-pro(turbo)", "openai", suffixChatWithEffort)
	if up["model"] != "deepseek-v4-pro" || up["reasoning_effort"] != "low" {
		t.Fatalf("unknown suffix must route and leave the request effort alone: %v", up)
	}
}

// A catalog ID that itself ends in parentheses resolves verbatim before
// any suffix interpretation.
func TestSuffixExactCatalogIDWins(t *testing.T) {
	m, f := newSuffixManager(t)
	up := upstreamJSON(t, m, f, "glm-x(beta)", "openai", suffixChatWithEffort)
	if up["model"] != "glm-x(beta)" || up["reasoning_effort"] != "low" {
		t.Fatalf("exact id must win with no override: %v", up)
	}
}

func TestSuffixOnUnknownBaseIs400(t *testing.T) {
	m, f := newSuffixManager(t)
	env := mustExecute(t, m, "deepseek-v9(high)", "openai", []byte(suffixChatWithEffort))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_model" || env.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if n := len(f.callsOf(pluginabi.MethodHostHTTPDo)); n != 1 { // the catalog fetch only
		t.Fatalf("an unknown base must never reach upstream: %d http calls", n)
	}
}

// On the Messages route the suffix only selects the model: there is no
// reasoning_effort to force, and the native body passes through untouched.
func TestSuffixOnMessagesRouteOnlySelectsModel(t *testing.T) {
	m, f := newSuffixManager(t)
	up := upstreamJSON(t, m, f, "minimax-m3(high)", "claude", suffixClaudePlain)
	if up["model"] != "minimax-m3" {
		t.Fatalf("upstream model = %v", up["model"])
	}
	if _, ok := up["reasoning_effort"]; ok {
		t.Fatalf("messages route must not gain reasoning_effort: %v", up)
	}
}

func TestSuffixAppliesOnStreamPath(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(suffixCatalog, streamResponder(streamScript{
		upstreamID: "up-sfx",
		frames:     []string{"data: [DONE]\n\n"},
	}))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("deepseek-v4-pro(max)", "claude",
			[]byte(`{"model":"x","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), "down-sfx"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("stream envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	var up map[string]any
	if err := json.Unmarshal(wireBody(t, lastWire(t, f, pluginabi.MethodHostHTTPDoStream), "body"), &up); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if up["model"] != "deepseek-v4-pro" || up["reasoning_effort"] != "max" || up["stream"] != true {
		t.Fatalf("stream upstream body = %v", up)
	}
}
