package rulesengine

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/expr-lang/expr/vm"
)

func TestCompileWhenEmptySourceReturnsNil(t *testing.T) {
	t.Parallel()
	cw, err := compileWhen("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if cw != nil {
		t.Fatal("empty source must return nil program")
	}
}

func TestCompileWhenSyntaxError(t *testing.T) {
	t.Parallel()
	if _, err := compileWhen("data."); err == nil {
		t.Fatal("invalid expression must error at compile")
	}
}

func TestCompileWhenEvaluatesAgainstData(t *testing.T) {
	t.Parallel()
	cw, err := compileWhen(`data.org != nil && data.plan == "pro"`)
	if err != nil {
		t.Fatalf("compile err = %v", err)
	}
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	c.Set("org", "acme")
	c.Set("plan", "pro")
	ok, err := cw.evaluate(c)
	if err != nil {
		t.Fatalf("evaluate err = %v", err)
	}
	if !ok {
		t.Fatal("expression must evaluate true")
	}

	c2 := &Context{data: &sync.Map{}, ctx: context.Background()}
	c2.Set("org", "acme")
	c2.Set("plan", "free")
	ok2, err := cw.evaluate(c2)
	if err != nil {
		t.Fatalf("evaluate err = %v", err)
	}
	if ok2 {
		t.Fatal("expression must evaluate false when plan != pro")
	}
}

func TestCompileWhenReadsRequest(t *testing.T) {
	t.Parallel()
	cw, err := compileWhen(`request.headers["X-Org-Id"] != ""`)
	if err != nil {
		t.Fatalf("compile err = %v", err)
	}
	req, _ := http.NewRequest("GET", "/foo", nil)
	req.Header.Set("X-Org-Id", "abc")
	c := &Context{data: &sync.Map{}, ctx: context.Background(), Request: req}
	ok, err := cw.evaluate(c)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestNewExprRequestRefNilSafe(t *testing.T) {
	t.Parallel()
	ref := newExprRequestRef(nil)
	if ref == nil || ref.Headers == nil {
		t.Fatal("nil request must yield a non-nil ref with empty Headers map")
	}
}

func TestNewExprRequestRefMissingURL(t *testing.T) {
	t.Parallel()
	r := &http.Request{Method: "GET", Header: http.Header{"X-Test": {"v"}}}
	ref := newExprRequestRef(r)
	if ref.Path != "" || ref.Method != "GET" || ref.Headers["X-Test"] != "v" {
		t.Fatalf("unexpected ref: %+v", ref)
	}
}

func TestEvaluateRuntimeError(t *testing.T) {
	t.Parallel()
	// Force a runtime nil-deref by reading a field of a nil map entry.
	cw, err := compileWhen(`data.org.id == "x"`)
	if err != nil {
		t.Fatalf("compile err = %v", err)
	}
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	c.Set("org", nil)
	if _, err := cw.evaluate(c); err == nil {
		t.Fatal("nil dereference at runtime must surface as an error")
	}
}

func TestCompileWhenSeamFailure(t *testing.T) {
	orig := compileWhenFn
	defer func() { compileWhenFn = orig }()
	compileWhenFn = func(string) (*vm.Program, error) {
		return nil, errors.New("forced compile failure")
	}
	if _, err := compileWhen("anything"); err == nil {
		t.Fatal("seam-injected compile failure must propagate")
	}
}

func TestEvaluateNonBoolResult(t *testing.T) {
	// Bypass compileWhen to install a program whose result isn't bool.
	// Easiest path: compile a numeric expression without AsBool() and
	// build the compiledWhen by hand.
	orig := compileWhenFn
	defer func() { compileWhenFn = orig }()
	compileWhenFn = func(source string) (*vm.Program, error) {
		// Compile WITHOUT AsBool() so result stays int.
		return compileIntForTest(source)
	}
	cw, err := compileWhen("1 + 2")
	if err != nil {
		t.Fatalf("compile err = %v", err)
	}
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	if _, err := cw.evaluate(c); err == nil {
		t.Fatal("non-bool result must error")
	}
}
