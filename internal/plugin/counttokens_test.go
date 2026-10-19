package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateRequestTokens_AnthropicMessages(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		min, max int
	}{
		{
			name: "英文按 4 字节/token",
			body: `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 400) + `"}]}`,
			min:  95, max: 115, // 400/4=100 + 角色/字段名等结构串
		},
		{
			name: "中文每字 1 token",
			body: `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("中", 100) + `"}]}`,
			min:  95, max: 115,
		},
		{
			name: "tool_result 里的文本要数进去",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"` +
				strings.Repeat("b", 400) + `"}]}]}]}`,
			min: 95, max: 130,
		},
		{
			name: "tool_use 的参数要数进去",
			body: `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Read","input":{"path":"` +
				strings.Repeat("c", 400) + `"}}]}]}`,
			min: 95, max: 130,
		},
		{
			name: "图片按块计、不按 base64 体积",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
				strings.Repeat("A", 40000) + `"}}]}]}`,
			min: 1000, max: 1060, // 1000 + 少量结构串；绝不能是 40000/4=10000
		},
		{
			name: "OpenAI chat 的 image_url 也按块计",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
				strings.Repeat("A", 40000) + `"}}]}]}`,
			min: 1000, max: 1060,
		},
		{
			name: "tools 定义要数进去",
			body: `{"model":"m","tools":[{"name":"Read","description":"` + strings.Repeat("e", 400) +
				`","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`,
			min: 95, max: 140,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateRequestTokens([]byte(tc.body))
			if got < tc.min || got > tc.max {
				t.Fatalf("estimateRequestTokens=%d, want %d..%d", got, tc.min, tc.max)
			}
		})
	}
}

func TestEstimateRequestTokens_NeverZero(t *testing.T) {
	for _, body := range []string{"", "not json", "{}", `{"messages":[]}`} {
		if got := estimateRequestTokens([]byte(body)); got < 1 {
			t.Fatalf("body=%q got=%d, want >=1", body, got)
		}
	}
}

func TestTokenCountPayload(t *testing.T) {
	if got := string(tokenCountPayload("claude", 42)); got != `{"input_tokens":42}` {
		t.Fatalf("claude payload=%s", got)
	}
	if got := string(tokenCountPayload(" openai ", 42)); got != `{"input_tokens":42}` {
		t.Fatalf("openai payload=%s", got)
	}
	var resp struct {
		Response struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(tokenCountPayload("openai-response", 42), &resp); err != nil {
		t.Fatalf("openai-response payload not json: %v", err)
	}
	if resp.Response.Usage.InputTokens != 42 || resp.Response.Usage.TotalTokens != 42 {
		t.Fatalf("openai-response usage=%+v", resp.Response.Usage)
	}
}

// 端到端：走 HandleCall 的 count_tokens 分支，不能返回 unsupported。
func TestHandleCountTokens_ReturnsCount(t *testing.T) {
	m := &Manager{}
	// OriginalRequest 是 []byte，SDK 按 Go 字段名序列化 → JSON 里是 base64。
	req, _ := json.Marshal(map[string]any{
		"Model":           "deepseek-v4.1-flash",
		"SourceFormat":    "claude",
		"OriginalRequest": []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello world"}]}`),
	})
	raw, err := m.HandleCall("executor.count_tokens", req)
	if err != nil {
		t.Fatalf("HandleCall err=%v", err)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope=%s err=%v", raw, err)
	}
	if env.Error != nil {
		t.Fatalf("count_tokens returned error envelope: %s", env.Error.Message)
	}
	// ExecutorResponse.Payload 是 []byte，信封里按 base64 编码。
	var resultPayload []byte
	if err := json.Unmarshal(env.Result.Payload, &resultPayload); err != nil {
		t.Fatalf("result payload=%s err=%v", env.Result.Payload, err)
	}
	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(resultPayload, &payload); err != nil {
		t.Fatalf("payload=%s err=%v", resultPayload, err)
	}
	if payload.InputTokens < 1 {
		t.Fatalf("input_tokens=%d, want >=1", payload.InputTokens)
	}
}
