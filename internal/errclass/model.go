package errclass

import (
	"fmt"
	"net/http"
)

// UnknownModel reports a requested model ID with no routable catalog entry
// as a 400 request error (ClassInvalidModel).
//
// It deliberately is not the upstream-style 404: CPA treats a 404 — and a
// 400/404/422 whose code or message reads like "model not found / not
// supported / unavailable" — as a model capability failure and cools the
// model down for 12 hours on that credential. A plugin-side miss is not
// that: the catalog may be empty mid-refresh, or the request carries an ID
// the catalog never had. A plain request fault keeps the credential and
// model available, and the client still gets a descriptive error. The
// message therefore avoids CPA's model-not-found phrases ("model not
// found", "unknown model", "does not exist", "not supported", ...), and
// invalid_model is not one of the codes CPA matches.
func UnknownModel(id string) *Error {
	return &Error{
		Class:      ClassInvalidModel,
		Message:    Redact(fmt.Sprintf("no routable OpenCode Go catalog entry for %q", id)),
		StatusCode: http.StatusBadRequest,
	}
}
