package plugin

import (
	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/errclass"
	"opencode-go-cliproxyapi/internal/thinking"
)

// lookupModel resolves a requested model ID against the catalog snapshot.
// The exact ID wins first, so a catalog ID that itself ends in parentheses
// still resolves verbatim. A miss on an ID carrying a CPA thinking suffix
// ("deepseek-v4-pro(high)" — CPA forwards the suffix to plugin executors
// as-is) retries with the suffix stripped and returns the suffix's
// reasoning-effort override, clamped to the resolved model's capability
// (thinking.ForModel: DeepSeek takes the full ladder).
func lookupModel(mgr *catalog.Manager, id string) (catalog.ModelRecord, thinking.Override, bool) {
	if rec, ok := mgr.Lookup(id); ok {
		return rec, thinking.Override{}, true
	}
	base, raw, ok := thinking.SplitSuffix(id)
	if !ok {
		return catalog.ModelRecord{}, thinking.Override{}, false
	}
	rec, ok := mgr.Lookup(base)
	if !ok {
		return catalog.ModelRecord{}, thinking.Override{}, false
	}
	return rec, thinking.SuffixOverride(raw, thinking.ForModel(rec.UpstreamID, rec.Thinking)), true
}

// upstreamRequest translates the client body for the resolved route and
// applies a model-suffix effort override on top. The suffix outranks the
// request's own effort (output_config.effort, thinking budget,
// reasoning.effort, reasoning_effort): it is the more deliberate choice —
// clients send their configured effort on every request, while a suffix is
// picked per model — and that is CPA's precedence for its built-in
// providers too. Only the Chat Completions route has a reasoning_effort to
// force; on the Messages and Responses routes the suffix only selects the
// model.
func (res *resolvedExecution) upstreamRequest(sourceFormat string, body []byte) ([]byte, *errclass.Error) {
	out, eErr := buildUpstreamRequest(res.rec.Protocol, res.rec.UpstreamID, sourceFormat, body, res.rec.Thinking, &res.tools)
	if eErr != nil || !res.effort.Set || res.rec.Protocol != catalog.RouteChatCompletions {
		return out, eErr
	}
	return chatcompletions.OverrideReasoningEffort(out, res.effort.Effort)
}
