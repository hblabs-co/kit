package rulesengine

// Rule is the unit of business logic the engine executes. Implement
// it on a struct that carries the dependencies the rule needs
// (datasource, config, clock, …) and register an instance with
// Engine.Register.
//
// Three methods, no surprises:
//
//   - Name returns the identifier YAML pipelines reference under
//     `name:`. Convention is snake_case — lowercase + underscore:
//     "fetch_organization", "load_plan", "set_access_flag". Must
//     be unique per Engine. The same convention applies to
//     pipeline names; nothing in the engine layer uses hyphens.
//   - Writes returns every key this rule sets in Context.Data.
//     The Engine uses this list at Build time to verify that no
//     two rules sharing a `parallel:` block declare overlapping
//     keys — that's how parallel execution stays race-free without
//     rule authors thinking about locks. Return nil if the rule
//     only reads or only signals via error.
//   - Run does the work. with carries the YAML `with:` config for
//     this specific step invocation; rules that don't take config
//     can ignore it. See errors.go for the contract on Skip and
//     Abort sentinels.
//
// Rules should be safe to share across goroutines — the Engine
// reuses the same instance for every request. Per-request state
// goes in Context.Data; per-step config arrives via the with
// parameter. Nothing on the receiver should mutate during Run.
type Rule interface {
	Name() string
	Writes() []string
	Run(c *Context, with StepConfig) error
}
