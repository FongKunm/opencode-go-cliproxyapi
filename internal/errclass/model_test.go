package errclass

import (
	"net/http"
	"strings"
	"testing"
)

func TestUnknownModelIsPlainRequestFault(t *testing.T) {
	e := UnknownModel("deepseek-v4-pro(foo)")
	if e.Class != ClassInvalidModel || e.StatusCode != http.StatusBadRequest || e.Retryable {
		t.Fatalf("UnknownModel = %+v, want non-retryable invalid_model 400", e)
	}
	wire := ToEnvelopeError(e)
	if wire.Code != "invalid_model" || wire.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("envelope = %+v", wire)
	}
	if !strings.Contains(e.Message, `"deepseek-v4-pro(foo)"`) {
		t.Fatalf("message must name the requested id: %q", e.Message)
	}
	// Phrases CPA's cooldown logic reads as a model capability failure
	// (sdk/cliproxy/auth/conductor_cooldown.go isModelSupportErrorMessage /
	// isExplicitModelNotFoundMessage, internal/clienterror model codes).
	lower := strings.ToLower(e.Message)
	for _, phrase := range []string{
		"model_not_supported", "requested model is not supported", "requested model is unsupported",
		"requested model is unavailable", "model is not supported", "model not supported",
		"unsupported model", "model unavailable", "not available for your plan",
		"not available for your account", "model_not_found", "unknown_model", "unknown model",
		"no such model", "not found", "does not exist", "doesn't exist", "is unknown",
	} {
		if strings.Contains(lower, phrase) {
			t.Fatalf("message %q contains cooldown trigger %q", e.Message, phrase)
		}
	}
	for _, prefix := range []string{"model", "the model", "requested model", "the requested model"} {
		if strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+":") {
			t.Fatalf("message %q starts with %q, which CPA parses for model-not-found", e.Message, prefix)
		}
	}
}

func TestUnknownModelRedacts(t *testing.T) {
	if msg := UnknownModel("Bearer sk-live-123").Message; strings.Contains(msg, "sk-live-123") {
		t.Fatalf("model id echo must be redacted: %q", msg)
	}
}
