package system

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sync"
	"testing"

	"github.com/hblabs-co/kit/core/constants"
)

// fakeLogger captures Info calls so Boot tests can assert on the
// build line without standing up real slog plumbing. *log.Logger
// satisfies the package's `logger` interface natively, but the spy
// pattern keeps the assertions tight.
type fakeLogger struct {
	mu    sync.Mutex
	infos []logEntry
}

type logEntry struct {
	msg  string
	args []any
}

func (f *fakeLogger) Info(msg string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos = append(f.infos, logEntry{msg: msg, args: args})
}

func (f *fakeLogger) infoCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.infos)
}

// only returns the entries with msg.
func (f *fakeLogger) only(msg string) []logEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []logEntry
	for _, e := range f.infos {
		if e.msg == msg {
			out = append(out, e)
		}
	}
	return out
}

// quietOpts is the test-only baseline: banner off, dotenv off,
// fake logger. Centralised so adding a new boot flag in the future
// doesn't fan out across every test fixture. Service name lives
// on bootOptions so each test passes its own.
func quietOpts(service string, fl *fakeLogger) *bootOptions {
	if fl == nil {
		fl = &fakeLogger{}
	}
	return BootOptions(service).With(
		WithoutBanner(),
		WithoutDotenv(),
		WithLogger(fl),
	)
}

// TestBoot_LogsBuildLine — when EnvBuildTime is set, Boot must emit
// exactly one Info("build", "time", <bt>) so rollouts are visible in
// the log stream. This is the only structured log Boot itself emits;
// regressions silently break ops' ability to confirm a deploy "took".
func TestBoot_LogsBuildLine(t *testing.T) {
	t.Setenv(constants.EnvBuildTime, "2026-05-08T10:00:00Z")
	fl := &fakeLogger{}

	ctx := Boot(quietOpts("test-svc", fl))
	if ctx == nil {
		t.Fatal("Boot returned nil ctx")
	}
	if got := len(fl.only("build")); got != 1 {
		t.Fatalf("build lines = %d, want 1", got)
	}
}

// TestBoot_NoBuildTimeNoLog — EnvBuildTime unset/whitespace must
// suppress the Info line. Otherwise local dev (`go run`) would emit
// a meaningless "build time=" line on every restart.
func TestBoot_NoBuildTimeNoLog(t *testing.T) {
	t.Setenv(constants.EnvBuildTime, "   ")
	fl := &fakeLogger{}

	Boot(quietOpts("test-svc", fl))
	if got := len(fl.only("build")); got != 0 {
		t.Errorf("build lines = %d, want 0 (build line should be skipped)", got)
	}
}

// TestBoot_ReturnsSameCtxAcrossCalls — Boot is documented as a
// process-singleton: bootOnce installs the signal handler exactly
// once, and subsequent calls return the same appCtx. Tests rely on
// this so a second Boot doesn't replace the cancellable ctx the
// caller already wired into RunForever.
func TestBoot_ReturnsSameCtxAcrossCalls(t *testing.T) {
	a := Boot(quietOpts("test-svc", nil))
	b := Boot(quietOpts("test-svc", nil))
	if a != b {
		t.Error("Boot returned a different ctx on the second call — bootOnce regression")
	}
}

// TestBoot_DotenvPathIsBestEffort — when WithoutDotenv is NOT set,
// Boot calls godotenv.Load. Missing .env is documented as fine
// (errors are ignored), so the test only needs to confirm Boot
// doesn't panic and still returns a valid ctx through that branch.
func TestBoot_DotenvPathIsBestEffort(t *testing.T) {
	// Run from a temp dir so any stray .env in the repo root can't
	// leak its keys into the test process.
	t.Chdir(t.TempDir())
	ctx := Boot(BootOptions("test-svc").With(
		WithoutBanner(),
		WithLogger(&fakeLogger{}),
	))
	if ctx == nil {
		t.Fatal("Boot returned nil ctx through dotenv path")
	}
}

// TestBoot_DefaultLoggerWhenUnset — when WithLogger is not applied,
// Boot builds the canonical service-tagged logger via core/log
// and installs it as the process default. Pin this branch so the
// "main can omit WithLogger and still get correlated logs" promise
// stays intact.
func TestBoot_DefaultLoggerWhenUnset(t *testing.T) {
	t.Setenv(constants.EnvBuildTime, "")
	t.Chdir(t.TempDir())
	captureSystemStdout(t, func() {
		ctx := Boot(BootOptions("svc-default").With(WithoutDotenv()))
		if ctx == nil {
			t.Fatal("Boot returned nil ctx through default-logger path")
		}
	})
}

// TestBoot_NilOptsPanics — service name is required (encoded in
// bootOptions via BootOptions), so passing nil is a
// programmer error. Panicking is the loud failure mode that
// surfaces the misuse at boot rather than silently substituting
// an empty service tag across every downstream log/metric.
func TestBoot_NilOptsPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Boot(nil) did not panic; want loud failure")
		}
	}()
	_ = Boot(nil)
}

// TestBoot_BannerPathPrints — without WithoutBanner, Boot must
// actually render the box; with Port it picks banner.Print
// (URLs) over banner.Box (title only). Capturing stdout proves both
// branches inside Boot run, not just the skip path. Also asserts
// the auto-prepended PID line surfaces — that's the one extra Boot
// owns directly and operators reach for it on every signal/grep.
func TestBoot_BannerPathPrints(t *testing.T) {
	out := captureSystemStdout(t, func() {
		Boot(BootOptions("svc-banner").With(
			WithoutDotenv(),
			Port(8080),
			BannerExtras("watching: ./pkg"),
			WithLogger(&fakeLogger{}),
		))
	})
	// Local URL appears only when Port triggered banner.Print.
	if !bytes.Contains([]byte(out), []byte("http://localhost:8080")) {
		t.Errorf("expected port URL in banner output:\n%s", out)
	}
	// Extras must appear too — covers BannerExtras flowing through.
	if !bytes.Contains([]byte(out), []byte("watching: ./pkg")) {
		t.Errorf("expected banner extras in output:\n%s", out)
	}
	// PID line is auto-prepended by Boot; assert the literal exact
	// PID for the running test process so a regression that drops
	// the line is caught (substring match on "PID:" alone would
	// false-pass on stray output). Use a regex to absorb the
	// banner's column-alignment padding between "PID:" and the
	// value — banner.Print pads to line up with "Local:" /
	// "Network:" so a literal-space match would re-break on the
	// next padding tweak.
	pidLine := regexp.MustCompile(fmt.Sprintf(`PID:\s+%d\b`, os.Getpid()))
	if !pidLine.MatchString(out) {
		t.Errorf("expected PID line for %d in banner output:\n%s", os.Getpid(), out)
	}
}

// TestBoot_BannerPathBoxOnly — banner enabled, no port → banner.Box
// (title-only) path. This is the "CLI tool" mode covered by the
// docstring; without this case the !skipBanner && port == 0 branch
// stays unexercised.
func TestBoot_BannerPathBoxOnly(t *testing.T) {
	out := captureSystemStdout(t, func() {
		Boot(BootOptions("svc-cli").With(
			WithoutDotenv(),
			WithLogger(&fakeLogger{}),
		))
	})
	if !bytes.Contains([]byte(out), []byte("svc-cli")) {
		t.Errorf("expected service name in box output:\n%s", out)
	}
	// banner.Box path must NOT print URL lines.
	if bytes.Contains([]byte(out), []byte("Local:")) {
		t.Errorf("Box path leaked Local URL:\n%s", out)
	}
}

// TestBootOptions_StashesService — service name reaches the
// struct field unchanged. Pin the constructor contract; downstream
// code (banner, log.New) reads opts.service so a corrupted value
// here would scatter wrong tags across every observability surface.
func TestBootOptions_StashesService(t *testing.T) {
	o := BootOptions("svc-x")
	if o.service != "svc-x" {
		t.Errorf("service = %q, want \"svc-x\"", o.service)
	}
}

// TestbootOptions_WithDispatchesEveryKnob — covers every case in
// the With switch by applying every constructor at once and
// asserting each lands in the right private field. One test for
// the whole table keeps regressions in the dispatcher visible
// without N tiny duplicate cases.
func TestBootOptions_WithDispatchesEveryKnob(t *testing.T) {
	fl := &fakeLogger{}
	o := BootOptions("svc").With(
		Port(8443),
		BannerExtras("a", "b"),
		WithoutBanner(),
		WithoutDotenv(),
		WithLogger(fl),
	)
	if o.port != 8443 {
		t.Errorf("port = %d, want 8443", o.port)
	}
	if !reflect.DeepEqual(o.bannerExtras, []string{"a", "b"}) {
		t.Errorf("bannerExtras = %v, want [a b]", o.bannerExtras)
	}
	if !o.skipBanner {
		t.Error("WithoutBanner did not flip skipBanner")
	}
	if !o.skipDotenv {
		t.Error("WithoutDotenv did not flip skipDotenv")
	}
	if o.logger != fl {
		t.Error("WithLogger did not assign the supplied logger")
	}
}

// TestbootOptions_WithAccumulatesBannerExtras — multiple With
// calls with BannerExtras must append, not replace. Composing
// extras from several sources (e.g. site.BannerExtras +
// service-specific hints) is a documented use case; locking it in
// catches regressions to the "last wins" semantic.
func TestBootOptions_WithAccumulatesBannerExtras(t *testing.T) {
	o := BootOptions("svc").
		With(BannerExtras("a", "b")).
		With(BannerExtras("c"))
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(o.bannerExtras, want) {
		t.Errorf("bannerExtras = %v, want %v", o.bannerExtras, want)
	}
}

// TestbootOptions_WithUnknownOptionPanics — unknown option names
// must crash loudly so a missing case in the dispatcher (or an
// option synthesised outside this package) surfaces immediately
// instead of silently no-op'ing.
func TestBootOptions_WithUnknownOptionPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("With(unknownOption) did not panic")
		}
	}()
	bogus := bootOption{name: "totally_not_a_real_knob", value: 0}
	BootOptions("svc").With(bogus)
}

// captureSystemStdout swaps os.Stdout for a pipe so banner output
// (fmt.Println) can be asserted on without polluting test output.
// Lives next to Boot tests because it's a system-package concern;
// the banner package has its own equivalent for its own tests.
func captureSystemStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	return buf.String()
}
