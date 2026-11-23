package scaleway

import (
	"log/slog"
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
	n := NewNotifier(client, from, Address{Email: strings.TrimSpace(to)})
	logNotifierDefaults(n, client)
	return n
}

// logNotifierDefaults logs, once at startup, what the notifier will
// use. Secrets (secret key, project ID) are reported only as set or
// unset, never by value. It goes to slog.Default(), which the kit's
// system.Boot points at the service logger. The message and the
// "area" attribute match the kit's other "effective defaults" lines.
func logNotifierDefaults(n *Notifier, c *Client) {
	var missing []string
	for _, k := range []string{EnvSecretKey, EnvProjectID, EnvMailFrom} {
		if env(k) == "" {
			missing = append(missing, k)
		}
	}
	region, regionSrc := c.Region, "env "+EnvRegion
	if env(EnvRegion) == "" {
		regionSrc = "default"
	}
	slog.Info("effective defaults", "area", "mail",
		"enabled", n.Enabled(),
		"to", n.to.Email, "from", n.from.Email, "from_name", n.from.Name,
		"region", region, "region_source", regionSrc,
		EnvSecretKey, setOrUnset(env(EnvSecretKey)),
		EnvProjectID, setOrUnset(env(EnvProjectID)),
		"missing", strings.Join(missing, ","))
}

func setOrUnset(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }
