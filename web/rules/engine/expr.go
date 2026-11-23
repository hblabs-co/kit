package rulesengine

import (
	"fmt"
	"net/http"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// compiledWhen is the compiled form of a `when:` expression: the
// VM program + the original source for error messages. The Engine
// keeps a map[string]*compiledWhen at boot so the same expression
// (deduped by source) compiles exactly once and is reused on every
// request.
type compiledWhen struct {
	source  string
	program *vm.Program
}

// exprEnv is the typed environment exposed to expr-lang. Fields:
//
//   - data: a snapshot of Context.Data at evaluation time. Maps in
//     expr support indexed and dot access, so authors can write
//     `data.organization != nil` or `data['plan'].tier == 'pro'`.
//   - request: the incoming HTTP request, narrowed to the fields
//     most useful in conditions (method, path, headers).
//
// Keep the surface small and obvious: the whole point of `when:` is
// to keep gnarly logic out of YAML — anything richer than a guard
// belongs in a rule.
type exprEnv struct {
	Data    map[string]any  `expr:"data"`
	Request *exprRequestRef `expr:"request"`
}

// exprRequestRef is a read-only adapter over *http.Request. We
// don't hand the full Request to expr-lang because (a) it pulls
// surface we don't want authors leaning on (Body, TLS, etc.) and
// (b) being explicit makes the supported variable set documentable.
type exprRequestRef struct {
	Method  string            `expr:"method"`
	Path    string            `expr:"path"`
	Headers map[string]string `expr:"headers"`
}

// newExprRequestRef builds the adapter once per pipeline Run. Headers
// flatten to first-value-per-key — expr authors writing
// `request.headers['X-Org-Id']` shouldn't have to know multi-value
// is even a thing.
func newExprRequestRef(r *http.Request) *exprRequestRef {
	if r == nil {
		return &exprRequestRef{Headers: map[string]string{}}
	}
	h := make(map[string]string, len(r.Header))
	for k, vs := range r.Header {
		if len(vs) > 0 {
			h[k] = vs[0]
		}
	}
	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}
	return &exprRequestRef{
		Method:  r.Method,
		Path:    path,
		Headers: h,
	}
}

// compileWhenFn is a variable seam: tests can swap it to force a
// compile failure on an otherwise-valid expression. Production
// always uses the real expr.Compile call.
var compileWhenFn = func(source string) (*vm.Program, error) {
	return expr.Compile(source, expr.Env(exprEnv{}), expr.AsBool())
}

// compileWhen wraps the seam with a friendlier error. Returns nil
// (no program) for an empty source so callers can `if cw == nil`
// to mean "step has no condition".
func compileWhen(source string) (*compiledWhen, error) {
	if source == "" {
		return nil, nil
	}
	prog, err := compileWhenFn(source)
	if err != nil {
		return nil, fmt.Errorf("rules: when=%q: %w", source, err)
	}
	return &compiledWhen{source: source, program: prog}, nil
}

// evaluate runs the compiled program against c. The env is built
// fresh per call (cheap: ~2 allocs) so the snapshot reflects the
// state of Data at the moment the executor reaches the step.
//
// A non-bool result from expr is coerced via the AsBool() option
// at compile time, so we can rely on the return being bool here.
// Any execution error propagates — `when:` errors block the step
// rather than silently letting it run.
func (cw *compiledWhen) evaluate(c *Context) (bool, error) {
	env := exprEnv{
		Data:    c.snapshot(),
		Request: newExprRequestRef(c.Request),
	}
	out, err := expr.Run(cw.program, env)
	if err != nil {
		return false, fmt.Errorf("rules: when=%q: %w", cw.source, err)
	}
	b, ok := out.(bool)
	if !ok {
		// Defensive: AsBool() should prevent this, but if expr's
		// type-coercion ever lets a non-bool through we'd rather
		// fail loudly than silently treat it as true/false.
		return false, fmt.Errorf("rules: when=%q: expected bool, got %T", cw.source, out)
	}
	return b, nil
}
