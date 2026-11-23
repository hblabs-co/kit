// Package rules is the shared catalog of Rule implementations for
// the rules engine in web/rules/engine. Every rule lives here
// regardless of how reusable it is in practice — keeping one home
// means services compose by importing this package + their own
// pipeline YAMLs, never by reaching into another service's tree.
//
// Each file is one rule. Add a new rule by dropping a new file
// here that exports a struct implementing the
// rulesengine.Rule interface, then register the instance from
// the service's pipelines wrapper (typically
// services/service-<name>/pipelines/pipelines.go).
//
// # Conventions
//
//   - One rule per file. Filename matches the rule's Name() in
//     snake_case (e.g. detect_locale.go ↔ "detect_locale"). Both
//     sides use lowercase + underscore — no hyphens in identifiers.
//   - Configuration arrives via the with StepConfig parameter,
//     never on the receiver. Rules are shared across goroutines.
//   - State sharing between rules happens through Context.Data
//     keys; declare them via Writes() so the engine can detect
//     parallel-write collisions at Build.
//
// # Quickstart for a service
//
//	import (
//	    rulesengine "github.com/hblabs-co/kit/web/rules/engine"
//	    "github.com/hblabs-co/kit/web/rules/rules"
//	)
//
//	eng := rulesengine.NewEngine().
//	    Register(&rules.DetectLocale{}, &rules.DetectTimeOfDay{}).
//	    Load(pipelinesFS, "pipelines/*.yaml")
//	if err := eng.Build(); err != nil { /* fail boot */ }
package rules
