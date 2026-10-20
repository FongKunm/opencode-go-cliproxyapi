package plugin

import (
	"bytes"

	"opencode-go-cliproxyapi/internal/adapter/sanitize"
	"opencode-go-cliproxyapi/internal/errclass"
)

// deriveSessionTolerant is deriveOpenCodeSessionID for request content the
// Chat Completions translator rewrites (internal/adapter/sanitize):
// derivation runs before translation and decodes the same content, so a
// document, search result or file-backed image in the history used to fail
// the request here before the rewrite could run. When the original body
// fails, the sanitized body is hashed instead; bodies that derive cleanly
// keep the exact id they always had.
func deriveSessionTolerant(sourceFormat string, body []byte) (string, *errclass.Error) {
	id, eErr := deriveOpenCodeSessionID(sourceFormat, body)
	if eErr == nil {
		return id, nil
	}
	var clean []byte
	switch sourceFormat {
	case "claude":
		clean = sanitize.ClaudeMessages(body)
	case "openai-response":
		clean = sanitize.Responses(body)
	}
	if clean == nil || bytes.Equal(clean, body) {
		return id, eErr
	}
	if retry, rErr := deriveOpenCodeSessionID(sourceFormat, clean); rErr == nil {
		return retry, nil
	}
	return id, eErr
}
