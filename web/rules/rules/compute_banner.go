package rules

import (
	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

// ComputeBannerFlags consumes the outputs of detect_locale and
// detect_time_of_day to decide which greeting banner the landing
// page should render. Demonstrates the "later rule reads earlier
// rule's keys" pattern that makes the engine useful for chaining
// business logic.
//
// Output shape (under the "banner" key):
//
//	{
//	  "greeting": "Buenos días",
//	  "tone":     "warm",
//	  "show_cta": true
//	}
type ComputeBannerFlags struct{}

// init wires ComputeBannerFlags into the package catalog. See
// catalog.go for the rationale.
func init() { Register(&ComputeBannerFlags{}) }

func (ComputeBannerFlags) Name() string     { return "compute_banner_flags" }
func (ComputeBannerFlags) Writes() []string { return []string{"banner"} }

// Run reads locale + time_of_day and builds the banner payload.
// Both inputs are guaranteed by the YAML's `when:` guard, so the
// rule treats their absence as a logic error.
func (ComputeBannerFlags) Run(c *rulesengine.Context, _ rulesengine.StepConfig) error {
	locale, _ := c.Get("locale")
	tod, _ := c.Get("time_of_day")

	greet := greetingFor(asString(locale), asString(tod))
	c.Set("banner", map[string]any{
		"greeting": greet,
		"tone":     toneFor(asString(tod)),
		"show_cta": asString(tod) != "night",
	})
	return nil
}

// greetingFor renders the per-locale greeting matched to the
// time-of-day bucket. Tables instead of switches so adding a
// locale is one line. Falls back to English when the locale is
// unmapped — same approach the i18n layer uses elsewhere.
func greetingFor(locale, tod string) string {
	table, ok := greetings[locale]
	if !ok {
		table = greetings["en"]
	}
	if g, ok := table[tod]; ok {
		return g
	}
	return table["morning"]
}

// toneFor maps the time bucket to a UI palette name the layout
// template understands. "warm" / "neutral" / "cool" / "dark".
func toneFor(tod string) string {
	switch tod {
	case "morning":
		return "warm"
	case "afternoon":
		return "neutral"
	case "evening":
		return "cool"
	default:
		return "dark"
	}
}

// asString is a tiny helper because Context.Get returns (any, bool)
// and rules don't need the bool when an earlier rule guaranteed
// the key is present.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// greetings is the locale → time-of-day → string table. Kept in
// the rule rather than a separate i18n file because these strings
// only flow through this rule and pulling them out would create
// indirection without payoff.
var greetings = map[string]map[string]string{
	"es": {"morning": "Buenos días", "afternoon": "Buenas tardes", "evening": "Buenas noches", "night": "Buenas noches"},
	"de": {"morning": "Guten Morgen", "afternoon": "Guten Tag", "evening": "Guten Abend", "night": "Gute Nacht"},
	"en": {"morning": "Good morning", "afternoon": "Good afternoon", "evening": "Good evening", "night": "Good night"},
	"nl": {"morning": "Goedemorgen", "afternoon": "Goedemiddag", "evening": "Goedenavond", "night": "Goedenacht"},
}
