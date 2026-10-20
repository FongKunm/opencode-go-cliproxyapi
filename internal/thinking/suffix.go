package thinking

import (
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// SplitSuffix splits a CPA thinking suffix off a model name
// ("deepseek-v4-pro(high)" → "deepseek-v4-pro", "high") using the host's
// own rule (internal/thinking/suffix.go ParseSuffix): the text after the
// last "(" up to a trailing ")". ok is false when the name carries no
// suffix. CPA forwards the suffix verbatim to plugin executors.
func SplitSuffix(model string) (base, raw string, ok bool) {
	open := strings.LastIndex(model, "(")
	if open < 0 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:open], model[open+1 : len(model)-1], true
}

// Override is the reasoning effort a model-name suffix forces on a
// request. The zero value forces nothing.
type Override struct {
	Set    bool   // the suffix carried a thinking configuration
	Effort string // effort to send; "" with Set removes the field (auto)
}

// SuffixOverride interprets a raw suffix value the way the host does
// (special values none/auto/-1 first, then the named levels, then
// non-negative integer budgets — all case-insensitive; anything else
// carries no configuration and forces nothing) and clamps the result to
// ts like any other client effort:
//
//   - a level → that level (Clamp);
//   - "none" or budget 0 → the off state (Clamp("none"));
//   - a positive budget → the host threshold table (budgetToLevel);
//   - "auto" or "-1" → no reasoning_effort at all, leaving the upstream to
//     decide (Chat Completions has no dynamic level).
func SuffixOverride(raw string, ts *pluginapi.ThinkingSupport) Override {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "auto", "-1":
		return Override{Set: true}
	}
	if level, ok := Clamp(v, ts); ok {
		return Override{Set: true, Effort: level}
	}
	budget, err := strconv.Atoi(v)
	if err != nil || budget < 0 {
		return Override{}
	}
	level := "none"
	if budget > 0 {
		level = budgetToLevel(int64(budget))
	}
	clamped, _ := Clamp(level, ts) // canonical by construction
	return Override{Set: true, Effort: clamped}
}
