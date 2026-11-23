package rules

import (
	"strings"

	"github.com/hblabs-co/kit/core/constants"
	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

// DetectLocale picks the best matching locale from the request's
// Accept-Language header. The rule is parametrised so the YAML
// can swap the supported set without re-deploying:
//
//	with:
//	  supported: [es, de, en, nl]
//	  default: de
//
// First two-letter prefix of an Accept-Language entry that appears
// in supported wins. Falls back to default. The output is written
// to Context.Data["locale"].
type DetectLocale struct{}

// init wires DetectLocale into the package catalog so any service
// that imports this package picks it up via rules.All() — no
// central list to keep in sync.
func init() { Register(&DetectLocale{}) }

// Name matches the YAML `name:` field. Kebab-case is the
// convention; one rule, one name, never aliased.
func (DetectLocale) Name() string { return "detect_locale" }

// Writes declares the keys this rule sets so Engine.Build can
// verify it doesn't collide with peers in a parallel group.
func (DetectLocale) Writes() []string { return []string{"locale"} }

// Run does the work. `with` carries the YAML config — supported
// locales and the fallback default. Both are required; missing
// config is treated as a service-config bug, not a request error,
// so the rule aborts cleanly with a reason rather than crashing.
func (DetectLocale) Run(c *rulesengine.Context, with rulesengine.StepConfig) error {
	supported := readStringSlice(with, "supported")
	if len(supported) == 0 {
		return rulesengine.Abort("detect_locale: supported list is empty")
	}
	def := with.String("default")
	if def == "" {
		def = supported[0]
	}

	accept := c.Request.Header.Get(constants.HeaderAcceptLanguage)
	c.Set("locale", pickLocale(accept, supported, def))
	return nil
}

// pickLocale walks the Accept-Language header in order and returns
// the first 2-letter prefix that's in supported. Falls back to def.
// Quality values (q=0.8) are ignored — the order in the header is
// already the priority signal browsers send, and the q-value parsing
// gets us into edge cases unrelated to the demo.
func pickLocale(accept string, supported []string, def string) string {
	if accept == "" {
		return def
	}
	for _, part := range strings.Split(accept, ",") {
		lang := strings.TrimSpace(part)
		if i := strings.IndexByte(lang, ';'); i >= 0 {
			lang = lang[:i]
		}
		if len(lang) < 2 {
			continue
		}
		prefix := strings.ToLower(lang[:2])
		for _, s := range supported {
			if strings.EqualFold(s, prefix) {
				return s
			}
		}
	}
	return def
}

// readStringSlice extracts a []string from a YAML-decoded
// map[string]any. yaml.v3 decodes a sequence of scalars into
// []any whose elements are strings; this helper converts without
// the rule having to spell out the casts.
func readStringSlice(with rulesengine.StepConfig, key string) []string {
	raw, ok := with[key]
	if !ok {
		return nil
	}
	seq, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(seq))
	for _, v := range seq {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
