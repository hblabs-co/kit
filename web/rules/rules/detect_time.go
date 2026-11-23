package rules

import (
	"time"

	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

// DetectTimeOfDay returns "morning" | "afternoon" | "evening" |
// "night" for the timezone declared in `with.timezone`. The
// landing page uses the result to tint a greeting banner without
// running JS for the same effect — a typical "compute on server,
// render once" use case.
//
// The current time source is a variable seam so tests can pin it.
type DetectTimeOfDay struct{}

// init wires DetectTimeOfDay into the package catalog. See
// catalog.go for the rationale.
func init() { Register(&DetectTimeOfDay{}) }

func (DetectTimeOfDay) Name() string     { return "detect_time_of_day" }
func (DetectTimeOfDay) Writes() []string { return []string{"time_of_day"} }

// Run is straightforward: load the configured timezone, ask Now()
// for the local hour, bucket. Falls back to UTC if the timezone is
// unknown — a deliberately quiet fallback so a typo in YAML doesn't
// take the page offline; the operator sees a wrong banner instead.
func (DetectTimeOfDay) Run(c *rulesengine.Context, with rulesengine.StepConfig) error {
	tz := with.String("timezone")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	hour := NowFn().In(loc).Hour()
	c.Set("time_of_day", bucketTimeOfDay(hour))
	return nil
}

// bucketTimeOfDay maps a 24-hour value to one of four buckets.
// The cut points are deliberately coarse — the goal is "feels
// right at a glance", not horological precision.
func bucketTimeOfDay(hour int) string {
	switch {
	case hour >= 5 && hour < 12:
		return "morning"
	case hour >= 12 && hour < 18:
		return "afternoon"
	case hour >= 18 && hour < 22:
		return "evening"
	default:
		return "night"
	}
}

// NowFn is the wall-clock seam exposed for tests. Cross-package
// tests (e.g. service-side pipeline E2E tests) can swap it to pin
// a deterministic time; production code never touches it. Exported
// (vs. unexported) precisely so other test packages can reach it
// — keeping the catalog tests deterministic at every level.
var NowFn = time.Now
