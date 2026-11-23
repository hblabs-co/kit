package clientmeta

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNormalizeIP_Canonical checks that mixed-case IPv6 collapses to
// the RFC 5952 canonical form. The store and the lookup both call
// NormalizeIP, so a mismatch here would silently break ban lookups.
func TestNormalizeIP_Canonical(t *testing.T) {
	cases := []struct {
		in, out string
		ok      bool
	}{
		{"", "", false},
		{"203.0.113.7", "203.0.113.7", true},
		{"2001:DB8::1", "2001:db8::1", true},
		{"not-an-ip", "not-an-ip", false},
	}
	for _, c := range cases {
		got, ok := NormalizeIP(c.in)
		if got != c.out || ok != c.ok {
			t.Errorf("NormalizeIP(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.out, c.ok)
		}
	}
}

// TestCapture_SnapshotsHeaders verifies every header lands on the
// canonical field. The point of the package is "capture verbatim",
// so we don't tolerate any silent dropping or massaging.
func TestCapture_SnapshotsHeaders(t *testing.T) {
	headers := map[string]string{
		"User-Agent":         "TestAgent/1.0",
		"Accept-Language":    "en-US,en;q=0.9",
		"X-App-Version":      "ios/2.4.1",
		"Sec-CH-UA":          `"Chromium";v="120"`,
		"Sec-CH-UA-Platform": `"macOS"`,
		"X-Real-IP":          "203.0.113.7",
		"X-Forwarded-For":    "203.0.113.7, 10.0.0.1",
		"CF-Ray":             "abcd1234-MAD",
		"CF-IPCountry":       "ES",
		"CF-Connecting-IP":   "2001:DB8::1",
		"CF-IPCity":          "Madrid",
		"CF-IPContinent":     "EU",
		"CF-IPLatitude":      "40.41650",
		"CF-IPLongitude":     "-3.70256",
	}
	get := func(k string) string { return headers[k] }
	m := Capture(get, "203.0.113.7")

	if m.IP != "203.0.113.7" {
		t.Errorf("IP = %q, want 203.0.113.7", m.IP)
	}
	if m.UserAgent != "TestAgent/1.0" {
		t.Errorf("UserAgent = %q", m.UserAgent)
	}
	if m.AppVersion != "ios/2.4.1" {
		t.Errorf("AppVersion = %q", m.AppVersion)
	}
	if m.SecCHUAPlatform != `"macOS"` {
		t.Errorf("SecCHUAPlatform = %q", m.SecCHUAPlatform)
	}
	if m.XForwardedFor != "203.0.113.7, 10.0.0.1" {
		t.Errorf("XForwardedFor = %q (raw chain expected, no parsing)", m.XForwardedFor)
	}
	// CF-Connecting-IP is the only field we canonicalise on the way in.
	if m.CFConnectingIP != "2001:db8::1" {
		t.Errorf("CFConnectingIP = %q, want canonical lower-case", m.CFConnectingIP)
	}
	if m.CFCountry != "ES" {
		t.Errorf("CFCountry = %q", m.CFCountry)
	}
	// Visitor-location headers — captured verbatim, lat/long kept
	// as strings (the package never parses them into floats).
	if m.CFCity != "Madrid" {
		t.Errorf("CFCity = %q, want Madrid", m.CFCity)
	}
	if m.CFContinent != "EU" {
		t.Errorf("CFContinent = %q, want EU", m.CFContinent)
	}
	if m.CFLatitude != "40.41650" {
		t.Errorf("CFLatitude = %q, want 40.41650 (verbatim string)", m.CFLatitude)
	}
	if m.CFLongitude != "-3.70256" {
		t.Errorf("CFLongitude = %q, want -3.70256 (verbatim string)", m.CFLongitude)
	}
}

// TestFromRequest_NetHTTP verifies the net/http convenience wrapper
// reads from r.Header and forwards clientIP unchanged.
func TestFromRequest_NetHTTP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "x")
	r.Header.Set("CF-IPCountry", "MX")

	m := FromRequest(r, "198.51.100.42")
	if m.IP != "198.51.100.42" {
		t.Errorf("IP = %q", m.IP)
	}
	if m.UserAgent != "x" || m.CFCountry != "MX" {
		t.Errorf("FromRequest didn't forward headers: %+v", m)
	}
}

// TestLogFields_OmitsEmpty makes sure logs stay compact — empty
// fields don't show up as "key=" noise. This is the contract the
// callers rely on when feeding the map into slog.Group / logrus.
func TestLogFields_OmitsEmpty(t *testing.T) {
	m := ClientMeta{IP: "1.1.1.1", UserAgent: "ua"}
	fields := m.LogFields()
	if len(fields) != 2 {
		t.Errorf("len(fields) = %d, want 2 (only ip + user_agent set)", len(fields))
	}
	if fields["ip"] != "1.1.1.1" || fields["user_agent"] != "ua" {
		t.Errorf("fields = %v", fields)
	}
	if _, ok := fields["cf_ray"]; ok {
		t.Errorf("empty cf_ray leaked into log fields")
	}
}

// TestLogFields_Geo guards the visitor-location fields: they must
// surface under their snake_case keys with the value passed through
// verbatim (no rounding / float coercion of lat/long).
func TestLogFields_Geo(t *testing.T) {
	m := ClientMeta{
		CFCity:      "Madrid",
		CFContinent: "EU",
		CFLatitude:  "40.41650",
		CFLongitude: "-3.70256",
	}
	fields := m.LogFields()
	want := map[string]string{
		"cf_city":      "Madrid",
		"cf_continent": "EU",
		"cf_latitude":  "40.41650",
		"cf_longitude": "-3.70256",
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("fields[%q] = %v, want %q", k, fields[k], v)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("len(fields) = %d, want %d (only geo fields set)", len(fields), len(want))
	}
}
