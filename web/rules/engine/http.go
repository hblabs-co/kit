package rulesengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/log"
)

// HTTPHandler returns an http.HandlerFunc that runs the named
// pipeline against the incoming request and serialises the Result
// as JSON. The handler is the wire-level entry point — composing
// it with the canonical middleware bundle (RequestID, Recover,
// AccessLog) is the caller's job:
//
//	mux.Handle("GET /access", eng.HTTPHandler("org_access_check"))
//
// Status code mapping:
//
//   - rule succeeded         → 200 OK + Result body
//   - rule returned Abort    → 403 Forbidden + {"reason": "...", "_abort": "..."}
//   - rule returned an error → 500 Internal Server Error + {"error": "..."}
//   - pipeline not found     → 500 (a missing pipeline is a config bug,
//     not a request bug — the operator gets the noisy log)
//
// Build() must have completed successfully before this handler
// serves traffic; if Build hasn't been called yet, the first
// request triggers it lazily (Run does this for us).
func (e *Engine) HTTPHandler(pipeline string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := e.Run(r.Context(), r, pipeline)
		writeJSON(w, r, pipeline, res, err)
	}
}

// writeJSON serialises res + err to w with the right status code.
// Pulled out so HTTPHandler stays a one-liner over Run + writeJSON
// — easier to read and to test the response shapes in isolation.
func writeJSON(w http.ResponseWriter, r *http.Request, pipeline string, res Result, err error) {
	w.Header().Set(constants.HeaderContentType, "application/json; charset=utf-8")

	if err == nil {
		writeBody(w, http.StatusOK, res)
		return
	}

	if IsAbort(err) {
		reason := AbortReason(err)
		body := Result{"reason": reason, "_abort": reason}
		// Echo the pipeline's pick-filtered Result so the caller can
		// still read whatever the pipeline produced before aborting
		// (e.g. the org's plan, used to render a "upgrade" UI).
		for k, v := range res {
			if _, taken := body[k]; !taken {
				body[k] = v
			}
		}
		writeBody(w, http.StatusForbidden, body)
		return
	}

	log.WithContext(r.Context()).LogAttrs(r.Context(), log.LevelError,
		"pipeline failed",
		log.String("pipeline", pipeline),
		log.String("error", err.Error()),
	)
	writeBody(w, http.StatusInternalServerError, Result{"error": engineErrorMessage(err)})
}

// engineErrorMessage extracts the message we put in the wire
// response. We don't expose internal RuleError formatting verbatim
// — the rule name is fine, but the inner message could leak stack
// traces or DB internals. Keep it generic when in doubt; the
// detailed string already went to the log above.
func engineErrorMessage(err error) string {
	var re *RuleError
	if errors.As(err, &re) {
		return "rule " + re.Rule + " failed"
	}
	return "pipeline failed"
}

// writeJSONFn is a variable seam so tests can force the json.Encode
// path to fail without a real broken ResponseWriter. Production
// always uses the real encoder.
var writeJSONFn = func(w http.ResponseWriter, body any) error {
	return json.NewEncoder(w).Encode(body)
}

// writeBody handles the status + body write. If encoding fails
// after WriteHeader (very unlikely for a Result that survived
// Marshal earlier in Run), we log; we can't recover the status code
// at that point.
func writeBody(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	if err := writeJSONFn(w, body); err != nil {
		// Best effort: status is already sent, so all we can do is log.
		log.Default().LogAttrs(context.Background(), log.LevelError,
			"rules: failed to write JSON body",
			log.String("error", err.Error()),
		)
	}
}
