package system

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// DefaultsMsg is the message of the "effective defaults" log lines
// that Boot and the kit's constructors emit once at startup, one per
// area (attribute "area"), each value paired with its source
// ("default", "env <KEY>", "option"). Secrets are never logged, only
// whether they are set.
const DefaultsMsg = "effective defaults"

// Source labels used in the defaults lines.
const (
	SourceDefault = "default"
	SourceOption  = "option"
)

// SourceEnv labels a value read from the environment variable key.
func SourceEnv(key string) string { return "env " + key }

// bootDefaults lists the system-level values in effect: listen
// address, environment (the service name is already on every
// line through the service logger), shutdown budget and drain delay.
func bootDefaults() []any {
	hbEnv := strings.TrimSpace(os.Getenv(constants.EnvHBEnv))
	envSrc := SourceEnv(constants.EnvHBEnv)
	if hbEnv == "" {
		hbEnv, envSrc = "local", SourceDefault
	}
	return []any{
		"area", "system",
		"listen_addr", ListenAddr(), "port_source", portSource(),
		"hb_env", hbEnv, "hb_env_source", envSrc,
		"shutdown_timeout", shutdownTimeout().String(),
		"shutdown_timeout_source", durSource(constants.EnvShutdownTimeout, false),
		"shutdown_delay", shutdownDelay().String(),
		"shutdown_delay_source", durSource(constants.EnvShutdownDelay, true),
	}
}

// portSource says where ListenPort's value comes from.
func portSource() string {
	if v := strings.TrimSpace(os.Getenv(constants.EnvPort)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return SourceEnv(constants.EnvPort)
		}
	}
	bootIdentity.RLock()
	defer bootIdentity.RUnlock()
	if bootIdentity.port > 0 {
		return SourceOption
	}
	return SourceDefault
}

// durSource reports whether the duration env key holds a valid value
// (zero allowed when allowZero).
func durSource(key string, allowZero bool) string {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return SourceDefault
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || (d == 0 && !allowZero) {
		return SourceDefault + " (invalid " + key + ")"
	}
	return SourceEnv(key)
}
