package plugin

import "testing"

func TestSessionHashAcceptsToolImages(t *testing.T) {
	request := func(data, toolName string) []byte {
		return []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"image"},{"type":"tool_reference","tool_name":"` + toolName + `"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}]}]}]}`)
	}
	a, err := deriveOpenCodeSessionID("claude", request("QUJD", "web_search"))
	if err != nil || a == "" {
		t.Fatalf("mixed image/reference result rejected: %v", err)
	}
	same, err := deriveOpenCodeSessionID("claude", request("QUJD", "web_search"))
	if err != nil || same != a {
		t.Fatalf("session hash is not stable: %v", err)
	}
	for _, body := range [][]byte{request("REVG", "web_search"), request("QUJD", "web_fetch")} {
		b, err := deriveOpenCodeSessionID("claude", body)
		if err != nil || a == b {
			t.Fatalf("changed content not represented in session: %v", err)
		}
	}
}
