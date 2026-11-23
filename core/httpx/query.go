package httpx

import (
	"net/url"
	"strconv"
)

// IntParam reads values[key] as a base-10 int. Empty values and
// values that fail to parse fall back to def — the caller picks a
// sensible default once and never has to branch on
// "missing vs malformed" again.
//
// Why "fall back" rather than "return error": the typical caller
// is a list / autocomplete endpoint where a malformed ?page=abc
// from a confused client should serve page 1, not 400 the request
// and break the UI. Endpoints that need strict parsing (eg a job
// id) should pull the raw value and validate explicitly.
func IntParam(values url.Values, key string, def int) int {
	s := values.Get(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
