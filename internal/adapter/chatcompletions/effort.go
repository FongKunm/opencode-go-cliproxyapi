package chatcompletions

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
	"opencode-go-cliproxyapi/internal/thinking"
)

// claudeReasoningEffort resolves the outbound reasoning_effort for a decoded
// Anthropic Messages request: the thinking control and output_config.effort
// combine per thinking.ClaudeEffort, clamped to ts (already widened by
// thinking.ForModel). ok is false when the field must be omitted.
func claudeReasoningEffort(src *shared.ClaudeRequestRecord, ts *pluginapi.ThinkingSupport) (string, bool) {
	thinkingType, budget := "", int64(0)
	if src.Thinking != nil {
		thinkingType, budget = src.Thinking.Type, src.Thinking.BudgetTokens
	}
	return thinking.ClaudeEffort(thinkingType, budget, src.OutputEffort, ts)
}

// OverrideReasoningEffort forces the reasoning_effort of an already built
// Chat Completions request body: effort replaces whatever the translation
// produced, and "" removes the field so the upstream default applies. It
// backs CPA model-name thinking suffixes ("deepseek-v4-pro(high)"), which
// outrank any effort carried in the request body — the host's own
// precedence for every built-in provider. Only the top level is decoded;
// nested values pass through byte-exact.
func OverrideReasoningEffort(body []byte, effort string) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		return nil, errclass.Translation("malformed Chat Completions request body")
	}
	if effort == "" {
		delete(req, "reasoning_effort")
	} else {
		raw, _ := json.Marshal(effort) // a string always marshals
		req["reasoning_effort"] = raw
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("Chat Completions request body cannot be re-encoded")
	}
	return out, nil
}
