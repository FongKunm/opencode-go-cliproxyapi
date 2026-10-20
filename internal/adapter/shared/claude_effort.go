package shared

import (
	"encoding/json"
	"strings"
)

// ClaudeOutputEffort extracts output_config.effort from an Anthropic
// Messages request: Claude Code pairs {"thinking":{"type":"adaptive"}} with
// {"output_config":{"effort":"..."}} for models that think adaptively, so
// the effort travels outside the thinking control. Decoding is tolerant —
// an absent, null, non-object, or non-string value yields "" — because the
// field is an optional hint and must never fail an otherwise valid request.
// The value is lowercased and trimmed; validation is the caller's job.
func ClaudeOutputEffort(raw json.RawMessage) string {
	if !HasContent(raw) {
		return ""
	}
	var cfg struct {
		Effort json.RawMessage `json:"effort"`
	}
	if json.Unmarshal(raw, &cfg) != nil || !HasContent(cfg.Effort) {
		return ""
	}
	var effort string
	if json.Unmarshal(cfg.Effort, &effort) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(effort))
}
