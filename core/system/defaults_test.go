package system

import (
	"fmt"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

func attrs(args []any) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		m[fmt.Sprint(args[i])] = fmt.Sprint(args[i+1])
	}
	return m
}

func TestBoot_LogsDefaults(t *testing.T) {
	t.Cleanup(func() { setBootIdentity("", 0) })
	t.Setenv(constants.EnvPort, "")
	t.Setenv(constants.EnvHBEnv, "")
	t.Setenv(constants.EnvShutdownTimeout, "")
	t.Setenv(constants.EnvShutdownDelay, "")
	fl := &fakeLogger{}
	Boot(quietOpts("svc-defaults", fl).With(Port(8087)))
	lines := fl.only(DefaultsMsg)
	if len(lines) != 1 {
		t.Fatalf("defaults lines = %d, want 1", len(lines))
	}
	got := attrs(lines[0].args)
	want := map[string]string{
		"area": "system", "listen_addr": ":8087", "port_source": SourceOption,
		"hb_env": "local", "hb_env_source": SourceDefault,
		"shutdown_timeout": "25s", "shutdown_timeout_source": SourceDefault,
		"shutdown_delay": "5s", "shutdown_delay_source": SourceDefault,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestBootDefaults_EnvSources(t *testing.T) {
	t.Cleanup(func() { setBootIdentity("", 0) })
	setBootIdentity("svc", 0)
	t.Setenv(constants.EnvPort, "9000")
	t.Setenv(constants.EnvHBEnv, "prod")
	t.Setenv(constants.EnvShutdownTimeout, "20s")
	t.Setenv(constants.EnvShutdownDelay, "0")
	got := attrs(bootDefaults())
	want := map[string]string{
		"listen_addr": ":9000", "port_source": "env HB_PORT", "hb_env": "prod", "hb_env_source": "env HB_ENV",
		"shutdown_timeout": "20s", "shutdown_timeout_source": "env HB_SHUTDOWN_TIMEOUT",
		"shutdown_delay": "0s", "shutdown_delay_source": "env HB_SHUTDOWN_DELAY",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	t.Setenv(constants.EnvPort, "")
	t.Setenv(constants.EnvShutdownTimeout, "0")
	t.Setenv(constants.EnvShutdownDelay, "nope")
	got = attrs(bootDefaults())
	if got["port_source"] != SourceDefault || got["shutdown_timeout_source"] != "default (invalid HB_SHUTDOWN_TIMEOUT)" ||
		got["shutdown_delay_source"] != "default (invalid HB_SHUTDOWN_DELAY)" {
		t.Errorf("fallback sources = %v", got)
	}
}
