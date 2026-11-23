package system

import (
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

func TestListenPortAndAddr(t *testing.T) {
	t.Cleanup(func() { setBootIdentity("", 0) })
	setBootIdentity("svc", 0)
	t.Setenv(constants.EnvPort, "")
	if ListenPort() != DefaultPort || ListenAddr() != ":8080" {
		t.Fatalf("default = %d %q", ListenPort(), ListenAddr())
	}
	setBootIdentity("svc", 8087)
	if ListenAddr() != ":8087" {
		t.Fatalf("boot port = %q", ListenAddr())
	}
	t.Setenv(constants.EnvPort, " 9001 ")
	if ListenAddr() != ":9001" {
		t.Fatalf("env port = %q", ListenAddr())
	}
	for _, bad := range []string{"x", "0", "-1"} {
		t.Setenv(constants.EnvPort, bad)
		if ListenAddr() != ":8087" {
			t.Errorf("HB_PORT=%q: %q, want the boot port", bad, ListenAddr())
		}
	}
}

func TestBoot_RecordsIdentity(t *testing.T) {
	t.Cleanup(func() { setBootIdentity("", 0) })
	t.Setenv(constants.EnvPort, "")
	Boot(BootOptions("svc-identity").With(WithoutDotenv(), WithoutBanner(), Port(8099), WithLogger(&fakeLogger{})))
	if ServiceName() != "svc-identity" || ListenAddr() != ":8099" {
		t.Fatalf("ServiceName=%q ListenAddr=%q", ServiceName(), ListenAddr())
	}
}
