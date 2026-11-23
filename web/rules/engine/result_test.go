package rulesengine

import (
	"reflect"
	"testing"
)

func TestResultPickSelected(t *testing.T) {
	t.Parallel()
	r := Result{"a": 1, "b": 2, "c": 3}
	got := r.Pick("a", "c", "missing")
	want := Result{"a": 1, "c": 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Pick = %v, want %v", got, want)
	}
}

func TestResultPickEmptyReturnsCopy(t *testing.T) {
	t.Parallel()
	r := Result{"a": 1}
	got := r.Pick()
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("Pick() copy = %v, want %v", got, r)
	}
	got["a"] = 99
	if r["a"] != 1 {
		t.Fatal("Pick() must return a copy, not the receiver")
	}
}

func TestResultBindToStruct(t *testing.T) {
	t.Parallel()
	r := Result{"access_granted": true, "plan": "pro", "extra": 42}
	var out struct {
		Access bool   `json:"access_granted"`
		Plan   string `json:"plan"`
	}
	if err := r.Bind(&out); err != nil {
		t.Fatalf("Bind err = %v", err)
	}
	if !out.Access || out.Plan != "pro" {
		t.Fatalf("Bind result = %+v", out)
	}
}

func TestResultBindMarshalError(t *testing.T) {
	t.Parallel()
	r := Result{"bad": make(chan int)} // channels are not JSON-marshalable
	if err := r.Bind(&struct{}{}); err == nil {
		t.Fatal("Bind must error when Result contains non-marshalable value")
	}
}

func TestResultBindUnmarshalError(t *testing.T) {
	t.Parallel()
	r := Result{"n": "not-an-int"}
	var out struct {
		N int `json:"n"`
	}
	if err := r.Bind(&out); err == nil {
		t.Fatal("Bind must error when destination type mismatches")
	}
}
