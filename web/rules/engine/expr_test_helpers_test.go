package rulesengine

import (
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// compileIntForTest compiles source without forcing AsBool, so the
// returned program produces an int / numeric value. Only used by
// TestEvaluateNonBoolResult to exercise the "expected bool, got X"
// branch in evaluate.
func compileIntForTest(source string) (*vm.Program, error) {
	return expr.Compile(source, expr.Env(exprEnv{}))
}
