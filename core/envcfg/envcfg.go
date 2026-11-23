// Package envcfg reads typed values from the process environment.
// Each helper falls back to a caller-supplied default when the
// variable is unset or unparseable, and logs a warning so a typo
// (e.g. PORT=eight-five) is visible in container logs instead of
// silently using the default.
//
// Single-call usage from main():
//
//	port := envcfg.Int("PORT", 8085)
//
// The package is intentionally tiny — every HB Labs service
// reads its config from a handful of vars, not a YAML schema, so we
// don't need viper/koanf machinery.
package envcfg

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// Int returns the integer value of the env var named key, falling
// back to def when unset or invalid. A non-integer value logs a
// warning so the operator sees the typo without the program dying.
func Int(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Warn("envcfg: invalid integer, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return n
}

// String returns the trimmed value of the env var named key, or def
// if the var is empty / unset. Whitespace-only values are treated as
// empty so a stray space in a .env file doesn't beat the default.
func String(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

// PositiveInt returns the integer value of the env var named key,
// constrained to be strictly positive (> 0). Falls back to def when
// the var is unset, unparseable, or non-positive — and logs a
// warning in the non-positive case so a misconfiguration like
// `HB_BATCH_SIZE=0` is visible in container logs instead of
// silently looping over a zero-sized batch forever.
//
// Use when a value's domain semantically excludes zero / negatives
// (batch sizes, page caps, worker counts). For values where zero
// is meaningful (disable a feature, "no cap"), reach for Int.
func PositiveInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Warn("envcfg: invalid integer, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	if n <= 0 {
		log.Warn("envcfg: integer must be positive, using default",
			"key", key, "value", n, "default", def)
		return def
	}
	return n
}

// Duration returns the Go-duration value of the env var named key,
// constrained to be strictly positive. Falls back to def when the
// var is unset, unparseable, or non-positive — non-positive values
// are almost always a misconfiguration (a zero interval loops hot,
// a negative one is nonsense) so the helper rejects them with a
// warning rather than passing them through.
//
// Format follows time.ParseDuration: "30s", "5m", "1h30m", etc.
func Duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Warn("envcfg: invalid duration, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	if d <= 0 {
		log.Warn("envcfg: duration must be positive, using default",
			"key", key, "value", d, "default", def)
		return def
	}
	return d
}

// Bool returns the boolean value of the env var named key, or def
// when unset / unparseable. Accepts the strconv.ParseBool grammar
// (1, t, T, TRUE, true, True / 0, f, F, FALSE, false, False).
func Bool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Warn("envcfg: invalid boolean, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return b
}

// ByteSize returns a byte count parsed from a Kubernetes-style quantity
// in the env var named key, falling back to def when unset, unparseable,
// or non-positive. Accepts binary suffixes (Ki, Mi, Gi, Ti), decimal
// suffixes (k, M, G, T), or a bare integer (bytes). Same convention as a
// k8s manifest's resource requests, so "1Mi", "512Ki", "10M" and
// "1048576" are all valid.
func ByteSize(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := parseByteSize(v)
	if err != nil {
		log.Warn("envcfg: invalid byte size, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	if n <= 0 {
		log.Warn("envcfg: byte size must be positive, using default",
			"key", key, "value", v, "default", def)
		return def
	}
	return n
}

// byteUnit pairs a suffix with its multiplier. Binary units (Ki, Mi, ...)
// are checked before the single-letter decimal ones so "Mi" never matches
// the decimal "M" branch.
type byteUnit struct {
	suffix string
	mul    int64
}

var byteUnits = []byteUnit{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"k", 1_000}, {"K", 1_000}, {"M", 1_000_000}, {"G", 1_000_000_000}, {"T", 1_000_000_000_000},
}

func parseByteSize(s string) (int64, error) {
	for _, u := range byteUnits {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return 0, err
			}
			return n * u.mul, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// Expand replaces every "${VAR}" and "${VAR:-default}" reference in s
// with values from the process environment, leaving bare "$NAME"
// alone. Used by config / catalog loaders that want per-environment
// overrides without dragging in a templating engine.
//
// Resolution: a set, non-empty env var wins; otherwise the ":-default"
// (if any) is used; otherwise the substitution is the empty string.
// An unterminated "${" passes through unchanged so a stray "$" in
// user copy doesn't take down the boot.
//
// We hand-roll the lexer (rather than reusing os.Expand) because
// os.Expand always consumes "$NAME" without braces too, with no way
// to opt out — and YAML scalars legitimately contain literal "$"
// (CSS selectors, jQuery, currency strings) that we don't want
// silently swallowed.
func Expand(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if i+1 >= len(s) || s[i] != '$' || s[i+1] != '{' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(resolveExpandName(s[i+2 : i+2+end]))
		i += 2 + end + 1
	}
	return b.String()
}

// resolveExpandName is the per-reference resolver: takes whatever sat
// inside "${...}" — either "VAR" or "VAR:-default" — and produces
// the substituted string. Pulled out so the parsing logic is
// trivially testable on its own.
func resolveExpandName(name string) string {
	envName, fallback, hasFallback := strings.Cut(name, ":-")
	if v, ok := os.LookupEnv(envName); ok && v != "" {
		return v
	}
	if hasFallback {
		return fallback
	}
	return ""
}
