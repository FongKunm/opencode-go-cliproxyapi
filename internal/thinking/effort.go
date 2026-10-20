package thinking

import (
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// IsDeepSeek reports whether an upstream model ID belongs to the DeepSeek
// family (ID prefix "deepseek", case-insensitive). Family-specific reasoning
// behavior keys off this single predicate so the rule cannot drift.
func IsDeepSeek(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// ForModel returns the effective thinking capability for an upstream model.
//
// DeepSeek accepts every canonical reasoning_effort level verbatim (none,
// minimal, low, medium, high, xhigh, max; upstream default high), but the
// OpenCode Go catalog publishes no levels for it, so the default
// low/medium/high ladder would silently cap "max" at "high" and turn "none"
// into "low". DeepSeek models therefore get the full canonical ladder plus
// the off state on top of whatever the catalog declared (Min/Max/Dynamic are
// kept). Every other family gets ts back unchanged: widening their ladders
// would forward levels nobody verified upstream.
func ForModel(model string, ts *pluginapi.ThinkingSupport) *pluginapi.ThinkingSupport {
	if !IsDeepSeek(model) {
		return ts
	}
	out := pluginapi.ThinkingSupport{}
	if ts != nil {
		out = *ts
	}
	out.Levels = slices.Clone(CanonicalLevels)
	out.ZeroAllowed = true
	return &out
}

// Clamp normalizes a client effort and maps it onto the nearest level ts
// supports (clampToSupported: index distance, weaker side on ties). The off
// state "none" is kept whenever ts allows turning reasoning off
// (ZeroAllowed) or declares it; otherwise it clamps to the weakest level
// like any other unsupported value. ok is false for values outside
// CanonicalLevels — including the dynamic "auto" — which callers omit
// instead of guessing.
func Clamp(effort string, ts *pluginapi.ThinkingSupport) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(effort))
	if !slices.Contains(CanonicalLevels, e) {
		return "", false
	}
	if e == "none" && ts != nil && ts.ZeroAllowed {
		return e, true
	}
	return clampToSupported(e, SupportedLevels(ts)), true
}

// ClaudeEffort resolves the reasoning_effort a Chat Completions upstream
// receives for an Anthropic Messages request (FR-005). Precedence:
//
//   - thinking.type "disabled" → the off state ("none", clamped to ts);
//   - output_config.effort (how Claude Code drives adaptive thinking:
//     {"type":"adaptive"} + {"effort":"low|medium|high|xhigh|max"}) → that
//     level clamped to ts, whatever the thinking type;
//   - thinking.type "enabled" → the budget mapping of EffortFromBudget,
//     unchanged from before;
//   - anything else ("adaptive" without an effort, no thinking control) →
//     ok is false: the field is omitted and the upstream default applies.
//
// An unrecognized output_config.effort value is ignored rather than
// rejected, falling through to the rules below it.
func ClaudeEffort(thinkingType string, budget int64, outputEffort string, ts *pluginapi.ThinkingSupport) (string, bool) {
	if thinkingType == "disabled" {
		return Clamp("none", ts)
	}
	if level, ok := Clamp(outputEffort, ts); ok {
		return level, true
	}
	if thinkingType == "enabled" {
		return EffortFromBudget(budget, ts), true
	}
	return "", false
}
