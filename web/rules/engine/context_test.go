package rulesengine

import (
	"context"
	"sync"
	"testing"
)

func TestNewContextRoundTrip(t *testing.T) {
	t.Parallel()
	c := NewContext(context.Background(), nil)
	if c.Context() == nil {
		t.Fatal("NewContext must populate ctx")
	}
	if c.Request != nil {
		t.Fatal("nil req must remain nil")
	}
}

func TestContextGetSet(t *testing.T) {
	t.Parallel()
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get on missing key must return ok=false")
	}
	c.Set("k", "v")
	got, ok := c.Get("k")
	if !ok || got != "v" {
		t.Fatalf("Get after Set = %v ok=%v", got, ok)
	}
}

func TestContextContextReturnsAttached(t *testing.T) {
	t.Parallel()
	parent := context.WithValue(context.Background(), ctxTestKey{}, "marker")
	c := &Context{data: &sync.Map{}, ctx: parent}
	if v, _ := c.Context().Value(ctxTestKey{}).(string); v != "marker" {
		t.Fatalf("Context() must return the attached ctx; got %v", v)
	}
}

type ctxTestKey struct{}

func TestContextSnapshotCopiesData(t *testing.T) {
	t.Parallel()
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	c.Set("a", 1)
	c.Set("b", "two")
	snap := c.snapshot()
	if snap["a"] != 1 || snap["b"] != "two" {
		t.Fatalf("snapshot = %v", snap)
	}
	snap["a"] = 99
	if v, _ := c.Get("a"); v != 1 {
		t.Fatal("snapshot must be a copy independent of Context")
	}
}

func TestContextSnapshotSkipsNonStringKeys(t *testing.T) {
	t.Parallel()
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	c.data.Store(42, "int-keyed")
	c.Set("ok", "v")
	snap := c.snapshot()
	if len(snap) != 1 || snap["ok"] != "v" {
		t.Fatalf("snapshot must skip non-string keys, got %v", snap)
	}
}

func TestContextConcurrentWritesDisjointKeys(t *testing.T) {
	t.Parallel()
	c := &Context{data: &sync.Map{}, ctx: context.Background()}
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Set(keyFor(i), i)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		v, ok := c.Get(keyFor(i))
		if !ok || v != i {
			t.Fatalf("Get(%d) = %v ok=%v", i, v, ok)
		}
	}
}

func keyFor(i int) string {
	return "k-" + string(rune('a'+(i%26))) + string(rune('0'+(i/26)))
}

func TestStepConfigTypedHelpers(t *testing.T) {
	t.Parallel()
	sc := StepConfig{
		"name":    "fetch",
		"enabled": true,
		"count":   3,
		"set":     nil,
	}
	if sc.String("name") != "fetch" {
		t.Fatal("String(name)")
	}
	if sc.String("missing") != "" {
		t.Fatal("String(missing) must be empty")
	}
	if sc.String("count") != "" {
		t.Fatal("String on non-string value must be empty")
	}
	if !sc.Bool("enabled") {
		t.Fatal("Bool(enabled)")
	}
	if sc.Bool("missing") {
		t.Fatal("Bool(missing) must be false")
	}
	if sc.Bool("name") {
		t.Fatal("Bool on non-bool must be false")
	}
	if sc.Int("count") != 3 {
		t.Fatal("Int(count)")
	}
	if sc.Int("missing") != 0 {
		t.Fatal("Int(missing) must be 0")
	}
	if sc.Int("enabled") != 0 {
		t.Fatal("Int on non-int must be 0")
	}
	if !sc.Has("set") {
		t.Fatal("Has(set) must be true even when value is nil")
	}
	if sc.Has("missing") {
		t.Fatal("Has(missing) must be false")
	}
}
