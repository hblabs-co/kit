package envcfg

import (
	"os"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// TestInt walks every branch: unset → default, valid → parsed,
// surrounding whitespace is trimmed, and a non-numeric value falls
// back to the default *and* surfaces a warning so the operator sees
// the typo (the whole reason this package exists over plain os.Getenv).
func TestInt(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		val       string
		def       int
		want      int
		warnMatch string // substring expected in stderr (log) when non-empty
	}{
		{name: "unset", set: false, def: 8085, want: 8085},
		{name: "empty", set: true, val: "", def: 8085, want: 8085},
		{name: "whitespace", set: true, val: "   ", def: 8085, want: 8085},
		{name: "valid", set: true, val: "9090", def: 8085, want: 9090},
		{name: "valid trimmed", set: true, val: "  4242  ", def: 8085, want: 4242},
		{name: "invalid", set: true, val: "eight-five", def: 8085, want: 8085, warnMatch: "envcfg: invalid integer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ENVCFG_TEST_INT", c.val)
			} else {
				// t.Setenv with empty would still set it; we want truly unset.
				unsetEnv(t, "ENVCFG_TEST_INT")
			}
			logs := log.Capture(t, "envcfg")
			got := Int("ENVCFG_TEST_INT", c.def)
			if got != c.want {
				t.Errorf("Int = %d, want %d", got, c.want)
			}
			log.AssertContains(t, logs, c.warnMatch)
		})
	}
}

// TestString covers the trim contract: a stray space in a .env file
// must not beat the default. Otherwise unset/empty fall back, and a
// real value passes through trimmed.
func TestString(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		def  string
		want string
	}{
		{name: "unset", set: false, def: "fallback", want: "fallback"},
		{name: "empty", set: true, val: "", def: "fallback", want: "fallback"},
		{name: "whitespace only", set: true, val: "   ", def: "fallback", want: "fallback"},
		{name: "value", set: true, val: "prod", def: "local", want: "prod"},
		{name: "value trimmed", set: true, val: "  prod  ", def: "local", want: "prod"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ENVCFG_TEST_STRING", c.val)
			} else {
				unsetEnv(t, "ENVCFG_TEST_STRING")
			}
			got := String("ENVCFG_TEST_STRING", c.def)
			if got != c.want {
				t.Errorf("String = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBool exercises the strconv.ParseBool grammar (1/0, t/f,
// TRUE/FALSE…) plus the warn-and-fallback path for anything outside
// it. "yes"/"no" intentionally do NOT parse — the doc on Bool ties
// behavior to strconv.ParseBool, so we lock that contract in.
func TestBool(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		val       string
		def       bool
		want      bool
		warnMatch string
	}{
		{name: "unset", set: false, def: true, want: true},
		{name: "empty", set: true, val: "", def: false, want: false},
		{name: "true", set: true, val: "true", def: false, want: true},
		{name: "TRUE upper", set: true, val: "TRUE", def: false, want: true},
		{name: "1", set: true, val: "1", def: false, want: true},
		{name: "false", set: true, val: "false", def: true, want: false},
		{name: "0", set: true, val: "0", def: true, want: false},
		{name: "trimmed", set: true, val: "  true  ", def: false, want: true},
		{name: "yes is invalid", set: true, val: "yes", def: false, want: false, warnMatch: "envcfg: invalid boolean"},
		{name: "garbage", set: true, val: "maybe", def: true, want: true, warnMatch: "envcfg: invalid boolean"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ENVCFG_TEST_BOOL", c.val)
			} else {
				unsetEnv(t, "ENVCFG_TEST_BOOL")
			}
			logs := log.Capture(t, "envcfg")
			got := Bool("ENVCFG_TEST_BOOL", c.def)
			if got != c.want {
				t.Errorf("Bool = %v, want %v", got, c.want)
			}
			log.AssertContains(t, logs, c.warnMatch)
		})
	}
}

// TestExpand walks every branch of the lexer: bare "$NAME" is left
// alone, "${VAR}" resolves against the environment, "${VAR:-default}"
// falls back, and an unterminated "${" passes through unchanged so a
// stray "$" in user copy doesn't take down the boot.
func TestExpand(t *testing.T) {
	t.Setenv("ENVCFG_TEST_EXPAND_SET", "value")
	t.Setenv("ENVCFG_TEST_EXPAND_EMPTY", "")
	unsetEnv(t, "ENVCFG_TEST_EXPAND_UNSET")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "no refs", in: "plain text", want: "plain text"},
		{name: "bare dollar untouched", in: "price $9.99", want: "price $9.99"},
		{name: "bare name untouched", in: "$PATH stays put", want: "$PATH stays put"},
		{name: "set var", in: "x=${ENVCFG_TEST_EXPAND_SET};", want: "x=value;"},
		{name: "empty var with fallback",
			in:   "x=${ENVCFG_TEST_EXPAND_EMPTY:-fb};",
			want: "x=fb;",
		},
		{name: "unset var with fallback",
			in:   "x=${ENVCFG_TEST_EXPAND_UNSET:-fb};",
			want: "x=fb;",
		},
		{name: "unset var no fallback",
			in:   "x=${ENVCFG_TEST_EXPAND_UNSET};",
			want: "x=;",
		},
		{name: "fallback ignored when set",
			in:   "x=${ENVCFG_TEST_EXPAND_SET:-fb};",
			want: "x=value;",
		},
		{name: "unterminated brace passes through",
			in:   "x=${unfinished",
			want: "x=${unfinished",
		},
		{name: "compose with literal",
			in:   "${ENVCFG_TEST_EXPAND_SET}/path/to/file",
			want: "value/path/to/file",
		},
		{name: "trailing dollar at end",
			in:   "abc$",
			want: "abc$",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Expand(c.in)
			if got != c.want {
				t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestPositiveInt walks every branch the helper covers: unset →
// default, valid positive → parsed, whitespace-trimmed, non-numeric
// warns and falls back, zero / negative warn and fall back. Zero is
// rejected with a distinct warning so a misconfiguration like
// HB_BATCH_SIZE=0 can't silently loop the worker over a zero-sized
// batch.
func TestPositiveInt(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		val       string
		def       int
		want      int
		warnMatch string
	}{
		{name: "unset", set: false, def: 10, want: 10},
		{name: "empty", set: true, val: "", def: 10, want: 10},
		{name: "whitespace only", set: true, val: "   ", def: 10, want: 10},
		{name: "valid positive", set: true, val: "42", def: 10, want: 42},
		{name: "valid trimmed", set: true, val: "  7  ", def: 10, want: 7},
		{name: "invalid", set: true, val: "ten", def: 10, want: 10, warnMatch: "envcfg: invalid integer"},
		{name: "zero rejected", set: true, val: "0", def: 10, want: 10, warnMatch: "envcfg: integer must be positive"},
		{name: "negative rejected", set: true, val: "-3", def: 10, want: 10, warnMatch: "envcfg: integer must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ENVCFG_TEST_POSINT", c.val)
			} else {
				unsetEnv(t, "ENVCFG_TEST_POSINT")
			}
			logs := log.Capture(t, "envcfg")
			got := PositiveInt("ENVCFG_TEST_POSINT", c.def)
			if got != c.want {
				t.Errorf("PositiveInt = %d, want %d", got, c.want)
			}
			log.AssertContains(t, logs, c.warnMatch)
		})
	}
}

// TestDuration exercises the Go-duration grammar, the trim
// contract, and the dual rejection of unparseable + non-positive
// values. The non-positive branch matters: a zero interval would
// loop hot inside periodic.Run, so defaulting silently would mask
// the misconfiguration. Warning + default makes it visible.
func TestDuration(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		val       string
		def       time.Duration
		want      time.Duration
		warnMatch string
	}{
		{name: "unset", set: false, def: 5 * time.Minute, want: 5 * time.Minute},
		{name: "empty", set: true, val: "", def: time.Minute, want: time.Minute},
		{name: "whitespace only", set: true, val: "   ", def: time.Minute, want: time.Minute},
		{name: "valid seconds", set: true, val: "30s", def: time.Minute, want: 30 * time.Second},
		{name: "valid composite", set: true, val: "1h30m", def: time.Minute, want: 90 * time.Minute},
		{name: "valid trimmed", set: true, val: "  10m  ", def: time.Minute, want: 10 * time.Minute},
		{name: "invalid", set: true, val: "5min", def: time.Minute, want: time.Minute, warnMatch: "envcfg: invalid duration"},
		{name: "zero rejected", set: true, val: "0s", def: time.Minute, want: time.Minute, warnMatch: "envcfg: duration must be positive"},
		{name: "negative rejected", set: true, val: "-5m", def: time.Minute, want: time.Minute, warnMatch: "envcfg: duration must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ENVCFG_TEST_DURATION", c.val)
			} else {
				unsetEnv(t, "ENVCFG_TEST_DURATION")
			}
			logs := log.Capture(t, "envcfg")
			got := Duration("ENVCFG_TEST_DURATION", c.def)
			if got != c.want {
				t.Errorf("Duration = %v, want %v", got, c.want)
			}
			log.AssertContains(t, logs, c.warnMatch)
		})
	}
}

// unsetEnv ensures key is absent for the duration of the test.
// t.Setenv has no "unset" mode, so we set-then-unset: t.Setenv
// registers a cleanup that restores the original process value, and
// the os.Unsetenv right after makes the var truly absent for the
// "unset" branch we want to exercise.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %s: %v", key, err)
	}
}

func TestByteSize(t *testing.T) {
	cases := []struct {
		name      string
		set       bool
		val       string
		def       int64
		want      int64
		warnMatch string
	}{
		{name: "unset", set: false, def: 1 << 20, want: 1 << 20},
		{name: "empty", set: true, val: "", def: 1 << 20, want: 1 << 20},
		{name: "bare bytes", set: true, val: "1048576", def: 0, want: 1048576},
		{name: "Ki", set: true, val: "512Ki", def: 0, want: 512 * 1024},
		{name: "Mi", set: true, val: "1Mi", def: 0, want: 1 << 20},
		{name: "Gi", set: true, val: "2Gi", def: 0, want: 2 << 30},
		{name: "Ti", set: true, val: "1Ti", def: 0, want: 1 << 40},
		{name: "decimal k", set: true, val: "1k", def: 0, want: 1000},
		{name: "decimal K", set: true, val: "1K", def: 0, want: 1000},
		{name: "decimal M", set: true, val: "10M", def: 0, want: 10_000_000},
		{name: "decimal G", set: true, val: "1G", def: 0, want: 1_000_000_000},
		{name: "decimal T", set: true, val: "1T", def: 0, want: 1_000_000_000_000},
		{name: "trimmed", set: true, val: "  1Mi  ", def: 0, want: 1 << 20},
		{name: "invalid suffix number", set: true, val: "xMi", def: 7, want: 7, warnMatch: "envcfg: invalid byte size"},
		{name: "garbage", set: true, val: "huge", def: 7, want: 7, warnMatch: "envcfg: invalid byte size"},
		{name: "zero rejected", set: true, val: "0", def: 7, want: 7, warnMatch: "envcfg: byte size must be positive"},
		{name: "negative rejected", set: true, val: "-1Mi", def: 7, want: 7, warnMatch: "envcfg: byte size must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ENVCFG_TEST_BYTES", c.val)
			} else {
				unsetEnv(t, "ENVCFG_TEST_BYTES")
			}
			logs := log.Capture(t, "envcfg")
			got := ByteSize("ENVCFG_TEST_BYTES", c.def)
			if got != c.want {
				t.Errorf("ByteSize = %d, want %d", got, c.want)
			}
			log.AssertContains(t, logs, c.warnMatch)
		})
	}
}
