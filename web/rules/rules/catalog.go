package rules

import (
	rulesengine "github.com/hblabs-co/kit/web/rules/engine"
)

// catalog is the package-level registry every rule file in this
// package appends to from its init(). The slice is mutated only
// during package initialisation; after main starts, All() returns
// stable copies and the underlying storage is never touched again.
//
// Unexported so external code can't mutate it — the only legitimate
// way to add a rule is to write a new file in this package and let
// its init() call Register.
var catalog []rulesengine.Rule

// Register appends r to the package-level catalog. Called from each
// rule file's init() so dropping a new file in this package wires
// it into every service that imports rules — no central list to
// keep in sync. Order across init() functions is undefined; that
// doesn't matter because Engine.Register uses a map.
//
// Exported so the registration is explicit (and grep-able). The
// alternative — magic reflection over package types — would hide
// the wiring and break with `go build -ldflags="-s"`-style dead-code
// elimination.
func Register(r rulesengine.Rule) {
	catalog = append(catalog, r)
}

// All returns every registered rule as a fresh slice so callers can
// append their own service-local rules without polluting the
// catalog:
//
//	eng := rulesengine.NewEngine().
//	    Register(rules.All()...).
//	    Register(&myService.SpecialRule{}).
//	    Load(yamlFS, "pipelines/*.yaml")
//
// Returning a copy (not the underlying array) is cheap — the
// catalog rarely exceeds a few dozen rules — and shields the
// registry from accidental mutation.
func All() []rulesengine.Rule {
	out := make([]rulesengine.Rule, len(catalog))
	copy(out, catalog)
	return out
}
