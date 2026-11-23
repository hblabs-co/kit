package httpx

import (
	"net/url"
	"testing"
)

// TestIntParam covers the three states the function distinguishes:
// missing key → default, malformed value → default, parseable
// value → that value. Table-driven so a future addition (eg
// negative defaults or scientific notation) extends one place.
func TestIntParam(t *testing.T) {
	cases := []struct {
		name   string
		values url.Values
		key    string
		def    int
		want   int
	}{
		{
			name:   "missing key falls back to default",
			values: url.Values{},
			key:    "page",
			def:    7,
			want:   7,
		},
		{
			name:   "empty value falls back to default",
			values: url.Values{"page": []string{""}},
			key:    "page",
			def:    7,
			want:   7,
		},
		{
			name:   "malformed value falls back to default",
			values: url.Values{"page": []string{"abc"}},
			key:    "page",
			def:    7,
			want:   7,
		},
		{
			name:   "parseable positive value is returned",
			values: url.Values{"page": []string{"42"}},
			key:    "page",
			def:    7,
			want:   42,
		},
		{
			name:   "parseable negative value is returned untouched",
			values: url.Values{"page": []string{"-3"}},
			key:    "page",
			def:    7,
			want:   -3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IntParam(tc.values, tc.key, tc.def)
			if got != tc.want {
				t.Errorf("IntParam = %d, want %d", got, tc.want)
			}
		})
	}
}
