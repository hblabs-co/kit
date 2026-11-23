// Package rules is a declarative pipeline engine: business logic
// lives in Rule structs (one file = one rule), composition lives
// in YAML, and the Engine sews them together at boot.
//
// # Why
//
// Inspired by the rule engine in dataplan.de's API. Same idea —
// each Rule is a single, named, reusable unit; pipelines stitch
// them into bigger flows — but the composition is declared in YAML
// instead of fluent Go calls. Two consequences worth knowing:
//
//   - The YAML can change the order of steps, add branches, skip
//     a rule under a condition (when:), without touching the Go
//     code that implements the rules.
//   - All validation happens at Engine.Build: missing rules,
//     overlapping writes inside a parallel group, unparseable
//     `when:` expressions — every error you can catch statically
//     surfaces at boot, not during the first request.
//
// # Flow
//
//	+----------+   Run(ctx, req, "name")   +-----------------+
//	| http     | ------------------------> | Engine          |
//	| handler  |                           |                 |
//	|          | <------ Result (dict) --- |   * registry    |
//	+----------+                           |   * pipelines   |
//	                                       |   * expr cache  |
//	                                       +-----------------+
//	                                                |
//	                                                v
//	                                       +-----------------+
//	                                       | Executor        |
//	                                       |   sequence ─┐   |
//	                                       |   parallel ─┤   |
//	                                       |   on_succ. ─┤   |
//	                                       |   on_fail. ─┘   |
//	                                       +-----------------+
//	                                                |
//	                                       per step v
//	                                       +-----------------+
//	                                       | Context         |
//	                                       |   .Request      |
//	                                       |   .Data (sync)  |
//	                                       |   .Logger       |
//	                                       +-----------------+
//
// # Quickstart
//
//	//go:embed pipelines/*.yaml
//	var pipelinesFS embed.FS
//
//	eng, err := rules.NewEngine().
//	    Register(
//	        &myrules.FetchOrganization{DS: ds},
//	        &myrules.LoadPlan{DS: ds},
//	        &myrules.SetAccessFlag{},
//	    ).
//	    Load(pipelinesFS, "pipelines/*.yaml").
//	    Build()
//	if err != nil { /* fail boot */ }
//
//	mux.Handle("GET /access", eng.HTTPHandler("org_access_check"))
//
//	// or inline, when the caller wants the Result to drive code:
//	res, err := eng.Run(r.Context(), r, "org_access_check")
//	var out AccessDecision
//	_ = res.Bind(&out)
//
// # Pipeline YAML
//
//	pipelines:
//	  org_access_check:
//	    pick: [access_granted, plan]
//	    flow:
//	      - parallel:
//	          - name: fetch_organization
//	            with: { source: header, key: X-Org-Id }
//	            on_failure:
//	              - name: deny_access
//	          - name: fetch_user
//	      - name: load_plan
//	        when: "data.organization != nil"
//	      - name: set_access_flag
//	        with: { min_plan: pro }
//
// Strict mode: unknown YAML keys fail at boot. Env interpolation
// (${HB_FOO:-default}) is applied before parsing — same syntax
// rules as web/render/config.
//
// # Rule contract
//
// A rule is a Go type that implements:
//
//	type Rule interface {
//	    Name() string         // matches `name:` in YAML
//	    Writes() []string     // keys this rule sets in Context.Data
//	    Run(c *Context) error // do the work
//	}
//
// Writes is what makes parallel groups safe by construction: at
// Build time, the Engine checks that no two rules in the same
// `parallel:` block declare a key in common. Race-free without
// rule authors having to think about it.
//
// Run signals:
//
//   - return nil           → success, next step runs
//   - return rules.Skip    → silently skip (when: conditions also
//     short-circuit before Run is even called)
//   - return rules.Abort("reason") → halt the pipeline; the reason
//     ends up in the Result and is mapped to a 4xx by HTTPHandler
//   - return err           → real failure; on_failure runs if set,
//     otherwise the pipeline stops and the error propagates
package rulesengine
