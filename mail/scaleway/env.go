package scaleway

import (
	"os"
	"strings"
)

// Standard environment keys read by NotifierFromEnv. The plain Client
// reads no environment; only this helper does.
const (
	EnvSecretKey = "HB_SCALEWAY_SECRET_KEY"
	EnvProjectID = "HB_SCALEWAY_PROJECT_ID"
	EnvRegion    = "HB_SCALEWAY_REGION"
	EnvMailFrom  = "HB_MAIL_FROM"
	// EnvMailFromName is the sender's display name (optional).
	EnvMailFromName = "HB_MAIL_FROM_NAME"
)

// NotifierFromEnv builds a Notifier that sends to the address to,
// configured from the standard keys above (region empty = the client
// default, fr-par). Missing credentials or sender yield a disabled
// notifier (Enabled() == false) rather than an error, so an app can
// boot without mail in local dev and show a fallback instead.
func NotifierFromEnv(to string) *Notifier {
	client := New(Options{
		SecretKey: env(EnvSecretKey),
		ProjectID: env(EnvProjectID),
		Region:    env(EnvRegion),
	})
	from := Address{Email: env(EnvMailFrom), Name: env(EnvMailFromName)}
	return NewNotifier(client, from, Address{Email: strings.TrimSpace(to)})
}

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }
