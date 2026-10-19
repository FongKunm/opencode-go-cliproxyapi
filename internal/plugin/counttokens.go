package plugin

// count_tokens 实现（我们 fork 后补的第一个能力，1020）。
//
// 官方插件的 executor 没有 count_tokens：Claude Code / Claude Desktop 每轮发来的
// POST /v1/messages/count_tokens 一律变成 400
// 「executor.count_tokens has no OpenCode Go equivalent」。
//
// 这不只是多一条报错——Claude Code 自动压缩的「执行」阶段会按消息分组逐个调
// count_tokens 来决定摘要哪些段，接口报错 → 分组失败 → 摘要算出来也应用不上，
// 上下文一路涨到上游 400。所以必须给它一个像样的数。
//
// OpenCode Go 没有 token 计数接口，这里按请求体本地估算：
//   - 只数会进模型的内容：文本、思考、工具参数、工具结果、工具定义；
//   - 图片（base64 或 data:image/ URL）按块固定 imageTokens，不按体积折；
//   - ASCII 4 字节/token，非 ASCII 每字 1 token。
//
// 实现刻意做成「格式无关」：递归遍历 JSON 的字符串值，不区分
// Anthropic Messages / OpenAI Chat / Responses 三种结构——三者都是把用户内容放在
// 字符串里，这样加新协议不用再写一份解析。

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// estimateBytesPerToken 是 ASCII 文本的保守折算率（≈4 字节/token）。
	estimateBytesPerToken = 4
	// imageTokens 是每张图片折算的输入 token（上游视觉输入按图块计，常见数百~数千）。
	imageTokens = 1000
	// base64MinLen 是判定「这是一段图片/二进制 base64」的最短长度。4KB 起判：
	// 正常文本不会出现 4KB 连续无空白的 base64 字符流。
	base64MinLen = 4096
)

// handleCountTokens 实现 executor.count_tokens。
func (m *Manager) handleCountTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed count tokens request body"), nil
	}
	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	n := estimateRequestTokens(body)
	debugTrace("count_tokens model=%s source_format=%s body_len=%d tokens=%d", req.Model, req.SourceFormat, len(body), n)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: tokenCountPayload(req.SourceFormat, n)}), nil
}

// tokenCountPayload 按客户端协议给出计数结果。
//
//   - claude / openai：Anthropic 的 count_tokens 契约就是 {"input_tokens":N}；
//   - openai-response：与 CPA 自带的 Codex 执行器同形，客户端从 response.usage 读。
func tokenCountPayload(sourceFormat string, n int) []byte {
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "openai-response":
		return []byte(fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, n, n))
	default:
		return []byte(fmt.Sprintf(`{"input_tokens":%d}`, n))
	}
}

// estimateRequestTokens 估算一次请求的输入 token。永远返回 ≥1（客户端会拿它算上下文比例，
// 返回 0 会让它以为上下文是空的）。
func estimateRequestTokens(body []byte) int {
	if len(body) == 0 {
		return 1
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		// 解析不了就退回按体积估，宁可粗也不能让客户端拿到 0。
		return maxInt(1, estimateTextTokens(string(body)))
	}
	return maxInt(1, estimateValueTokens(v))
}

// estimateValueTokens 递归统计 JSON 里所有字符串值的 token。
func estimateValueTokens(v any) int {
	switch x := v.(type) {
	case string:
		if isImagePayload(x) {
			return imageTokens
		}
		return estimateTextTokens(x)
	case []any:
		total := 0
		for _, e := range x {
			total += estimateValueTokens(e)
		}
		return total
	case map[string]any:
		total := 0
		for _, e := range x {
			total += estimateValueTokens(e)
		}
		return total
	}
	return 0
}

// isImagePayload 判定一个字符串是不是图片数据（base64 或 data:image/ URL）。
func isImagePayload(s string) bool {
	if strings.HasPrefix(s, "data:image/") {
		return true
	}
	if len(s) < base64MinLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isBase64Byte(s[i]) {
			return false
		}
	}
	return true
}

// isBase64Byte 报告 b 是否属于 base64 字符集（标准 + URL-safe 变体 + 填充）。
func isBase64Byte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' ||
		b == '+' || b == '/' || b == '-' || b == '_' || b == '='
}

// estimateTextTokens 按 ASCII 4 字节/token、其它字符每字 1 token 估算。
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, other := 0, 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
		i += size
	}
	return (ascii+estimateBytesPerToken-1)/estimateBytesPerToken + other
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
