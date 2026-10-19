package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Exercise both executor entry points with a mock host: no external API or
// model vision capability is assumed by these wire-format regression tests.
func TestExecuteClaudeImageToolResults(t *testing.T) {
	const body = `{
  "messages":[
   {"role":"user","content":"Compare the tool images."},
   {"role":"assistant","content":[
    {"type":"tool_use","id":"a","name":"Read","input":{}},
    {"type":"tool_use","id":"b","name":"Read","input":{}}
   ]},
   {"role":"user","content":[
    {"type":"tool_result","tool_use_id":"a","content":[
     {"type":"text","text":"caption"},
     {"type":"tool_reference","tool_name":"web_search"},
     {"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}
    ]},
    {"type":"tool_result","tool_use_id":"b","is_error":true,"content":[
     {"type":"tool_reference"},
     {"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}
    ]}
   ]}
  ]
 }`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var m *Manager
			var f *fakeCaller
			method, hostMethod := "executor.execute", pluginabi.MethodHostHTTPDo
			if stream {
				m, f = newStreamManager(t, streamScript{upstreamID: "image-up", frames: []string{
					`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}` + "\n\n",
					`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
					"data: [DONE]\n\n",
				}})
				method, hostMethod = "executor.execute_stream", pluginabi.MethodHostHTTPDoStream
			} else {
				m, f = newExecManager(t)
			}
			request := strings.Replace(body, `"messages":`, fmt.Sprintf(`"stream":%t,"messages":`, stream), 1)
			args := execReqBody("opencode-go/glm-5.3", "claude", []byte(request), false)
			if stream {
				args = execStreamReqBody("opencode-go/glm-5.3", "claude", []byte(request), "image-down")
			}
			response, err := m.HandleCall(method, args)
			if err != nil {
				t.Fatal(err)
			}
			env := decodeEnv(t, response)
			if !env.OK {
				t.Fatalf("image tool result failed: %+v", env.Error)
			}
			if !m.bridge.WaitForInFlight(5 * time.Second) {
				t.Fatal("host calls did not finish")
			}
			assertImageToolWire(t, wireBody(t, lastWire(t, f, hostMethod), "body"), stream)
			if stream {
				var events strings.Builder
				for _, call := range f.callsOf(pluginabi.MethodHostStreamEmit) {
					events.Write(wireBody(t, decodePayload(t, call), "payload"))
				}
				for _, want := range []string{"message_start", "content_block_delta", "hello", "message_stop"} {
					if !strings.Contains(events.String(), want) {
						t.Fatalf("missing %q in translated stream: %s", want, events.String())
					}
				}
				closes := f.callsOf(pluginabi.MethodHostStreamClose)
				if len(closes) != 1 || strings.Contains(string(closes[0].payload), `"error"`) {
					t.Fatal("stream did not close cleanly")
				}
			} else {
				var out pluginapi.ExecutorResponse
				if err := json.Unmarshal(env.Result, &out); err != nil {
					t.Fatal(err)
				}
				var message struct {
					Type       string `json:"type"`
					StopReason string `json:"stop_reason"`
					Content    []struct {
						Text string `json:"text"`
					} `json:"content"`
				}
				if err := json.Unmarshal(out.Payload, &message); err != nil {
					t.Fatal(err)
				}
				if message.Type != "message" || message.StopReason != "end_turn" || len(message.Content) != 1 || message.Content[0].Text != "hello" {
					t.Fatalf("invalid Claude reply: %s", out.Payload)
				}
			}
		})
	}
}

func assertImageToolWire(t *testing.T, body []byte, stream bool) {
	t.Helper()
	var request struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.Stream != stream || len(request.Messages) != 5 {
		t.Fatalf("invalid request: %s", body)
	}
	for i, id := range []string{"a", "b"} {
		reply := request.Messages[i+2]
		if reply.Role != "tool" || reply.ToolCallID != id {
			t.Fatalf("parallel replies must precede user images: %s", body)
		}
		var text string
		if err := json.Unmarshal(reply.Content, &text); err != nil {
			t.Fatalf("tool content must be text: %v", err)
		}
		want := "caption[tool: web_search]"
		if i == 1 {
			want = "[error] [tool reference]"
		}
		if !strings.HasPrefix(text, want) {
			t.Fatalf("lost text/reference/error status: %q", text)
		}
	}
	imageMessage := request.Messages[4]
	if imageMessage.Role != "user" {
		t.Fatalf("images require user role: %s", body)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(imageMessage.Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 4 {
		t.Fatalf("lost image association: %s", imageMessage.Content)
	}
	for i, id := range []string{"a", "b"} {
		if parts[2*i].Type != "text" || !strings.Contains(parts[2*i].Text, "tool_call_id "+id) || parts[2*i+1].Type != "image_url" {
			t.Fatalf("image not associated with tool %s: %s", id, imageMessage.Content)
		}
	}
	if parts[1].ImageURL.URL != "data:image/png;base64,QUJD" || parts[3].ImageURL.URL != "https://example.com/b.png" {
		t.Fatalf("image data changed: %s", imageMessage.Content)
	}
}
