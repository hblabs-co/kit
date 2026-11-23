package scaleway

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureLog points slog.Default at a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestNotifierFromEnv(t *testing.T) {
	for _, k := range []string{EnvSecretKey, EnvProjectID, EnvRegion, EnvMailFrom, EnvMailFromName} {
		t.Setenv(k, "")
	}
	if NotifierFromEnv("inbox@example.com").Enabled() {
		t.Fatal("enabled without configuration")
	}
	t.Setenv(EnvSecretKey, " sk ")
	t.Setenv(EnvProjectID, "pid")
	t.Setenv(EnvRegion, "nl-ams")
	t.Setenv(EnvMailFrom, "no-reply@example.com")
	t.Setenv(EnvMailFromName, "Site")
	n := NotifierFromEnv(" inbox@example.com ")
	if !n.Enabled() || n.client.SecretKey != "sk" || n.client.ProjectID != "pid" || n.client.Region != "nl-ams" ||
		n.from != (Address{Email: "no-reply@example.com", Name: "Site"}) || n.to.Email != "inbox@example.com" {
		t.Fatalf("notifier = %+v client = %+v", n, n.client)
	}
	if NotifierFromEnv("").Enabled() {
		t.Fatal("enabled without a recipient")
	}
}

// TestEnvKeyValues pins the deployed key names.
func TestEnvKeyValues(t *testing.T) {
	want := map[string]string{
		EnvSecretKey: "HB_SCALEWAY_SECRET_KEY", EnvProjectID: "HB_SCALEWAY_PROJECT_ID",
		EnvRegion: "HB_SCALEWAY_REGION", EnvMailFrom: "HB_MAIL_FROM", EnvMailFromName: "HB_MAIL_FROM_NAME",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("%q != %q", got, w)
		}
	}
}

// TestNotifierFromEnv_LogNeverLeaksSecrets: the defaults line reports
// the secret key and project ID only as set/unset.
func TestNotifierFromEnv_LogNeverLeaksSecrets(t *testing.T) {
	const secret, project = "SECRET-scw-0123456789", "PROJECT-abcdef-42"
	buf := captureLog(t)
	for _, k := range []string{EnvSecretKey, EnvProjectID, EnvRegion, EnvMailFrom, EnvMailFromName} {
		t.Setenv(k, "")
	}
	NotifierFromEnv("inbox@example.com")
	t.Setenv(EnvSecretKey, secret)
	t.Setenv(EnvProjectID, project)
	t.Setenv(EnvMailFrom, "no-reply@example.com")
	t.Setenv(EnvRegion, "nl-ams")
	NotifierFromEnv("inbox@example.com")
	out := buf.String()
	if strings.Contains(out, secret) || strings.Contains(out, project) {
		t.Fatalf("secret value in log:\n%s", out)
	}
	for _, want := range []string{
		"area=mail", "enabled=false", "missing=HB_SCALEWAY_SECRET_KEY,HB_SCALEWAY_PROJECT_ID,HB_MAIL_FROM",
		"HB_SCALEWAY_SECRET_KEY=unset", "region=fr-par", "region_source=default",
		"enabled=true", "HB_SCALEWAY_SECRET_KEY=set", "HB_SCALEWAY_PROJECT_ID=set", `missing=""`,
		"region=nl-ams", "region_source=\"env HB_SCALEWAY_REGION\"", "to=inbox@example.com", "from=no-reply@example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}
