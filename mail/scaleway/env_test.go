package scaleway

import "testing"

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
