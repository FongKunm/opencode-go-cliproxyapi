package chatcompletions

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
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
