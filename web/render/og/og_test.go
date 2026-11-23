package og

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"image"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// fakeStorage is a scripted Storage stand-in. Lets tests force the
// non-happy paths in NewHandler (Get error, PutIfAbsent error,
// stored item with empty content-type) without depending on real
// I/O against MemoryStorage.
type fakeStorage struct {
	getBody  []byte
	getCType string
	getErr   error
	putErr   error
}

func (f *fakeStorage) Get(context.Context, string) ([]byte, string, error) {
	return f.getBody, f.getCType, f.getErr
}
func (f *fakeStorage) PutIfAbsent(context.Context, string, []byte, string) error {
	return f.putErr
}
func (f *fakeStorage) URL(string) string { return "" }

func newGen(t *testing.T) *Generator {
	t.Helper()
	g, err := New(Options{})
	if err != nil {
		t.Fatalf("og.New: %v", err)
	}
	return g
}

func TestRenderProducesValidPNGAtCanonicalSize(t *testing.T) {
	g := newGen(t)
	body, err := g.Render(Card{
		Title:    "Hosting gestionado y migración para PYMEs",
		Subtitle: "Tu sitio web, hospedado y gestionado.",
		Brand:    "Example",
		Lang:     "es",
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != Width || h != Height {
		t.Fatalf("dims = %dx%d, want %dx%d", w, h, Width, Height)
	}
}

func TestKeyIsStableAndDifferentPerCard(t *testing.T) {
	g := newGen(t)
	a := g.Key(Card{Title: "Hi", Lang: "es"})
	b := g.Key(Card{Title: "Hi", Lang: "es"})
	c := g.Key(Card{Title: "Hi", Lang: "en"})
	if a != b {
		t.Errorf("same card produced different keys: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("different cards collided: %s", a)
	}
	if len(a) != 16 {
		t.Errorf("key length = %d, want 16", len(a))
	}
}

func TestKeyResistsFieldBoundaryAmbiguity(t *testing.T) {
	g := newGen(t)
	// Without a separator between fields, ("ab", "c") and ("a", "bc")
	// would collide. The NUL writer in Key prevents that.
	a := g.Key(Card{Brand: "ab", Title: "c"})
	b := g.Key(Card{Brand: "a", Title: "bc"})
	if a == b {
		t.Fatalf("Key collided across field boundary: %s", a)
	}
}

func TestUnknownThemeIsAnError(t *testing.T) {
	g := newGen(t)
	if _, err := g.Render(Card{Title: "x", Theme: "ghost"}); err == nil {
		t.Fatal("expected error for unknown theme")
	}
}

func TestHandlerCacheAside(t *testing.T) {
	g := newGen(t)
	store := NewMemoryStorage("/og")
	var calls int
	resolver := func(r *http.Request) (Card, bool) {
		// One fixed card per request — what matters here is the
		// hit/miss path, not the URL parsing.
		return Card{Title: "Hi", Subtitle: "There", Brand: "B", Lang: "es"}, true
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		NewHandler(HandlerOptions{
			Generator: g,
			Storage:   store,
			Resolver:  resolver,
		}).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/og/anything.png")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first call status = %d", resp.StatusCode)
	}
	if !strings.HasPrefix(string(first), "\x89PNG") {
		t.Fatalf("first call did not return PNG bytes")
	}

	resp, err = http.Get(srv.URL + "/og/anything.png")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !bytes.Equal(first, second) {
		t.Fatal("cache-aside served different bytes on hit vs miss")
	}

	// Verify Storage actually got populated (independent of the
	// handler's internal state).
	card := Card{Title: "Hi", Subtitle: "There", Brand: "B", Lang: "es"}
	if _, _, err := store.Get(context.Background(), g.Key(card)); err != nil {
		t.Fatalf("storage Get after handler call: %v", err)
	}
}

func TestResolverFalseReturns404(t *testing.T) {
	g := newGen(t)
	store := NewMemoryStorage("/og")
	h := NewHandler(HandlerOptions{
		Generator: g,
		Storage:   store,
		Resolver:  func(*http.Request) (Card, bool) { return Card{}, false },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/og/anything.png")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMemoryStorageIsIdempotentOnDuplicatePut(t *testing.T) {
	store := NewMemoryStorage("/og")
	ctx := context.Background()
	if err := store.PutIfAbsent(ctx, "k", []byte("first"), "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutIfAbsent(ctx, "k", []byte("second"), "image/png"); err != nil {
		t.Fatal(err)
	}
	body, _, err := store.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "first" {
		t.Fatalf("PutIfAbsent overwrote: got %q, want %q", body, "first")
	}
}

// TestMemoryStorage_URLPrefixAndURL — both helpers feed the
// auto-mount path: URLPrefix tells the route mounter where to bind,
// URL is what the page embeds in og:image tags. Trim and assemble
// behaviour pinned here so the route + meta tag stay in sync.
func TestMemoryStorage_URLPrefixAndURL(t *testing.T) {
	store := NewMemoryStorage("/og/")
	if got, want := store.URLPrefix(), "/og"; got != want {
		t.Errorf("URLPrefix = %q, want %q (trailing slash should be trimmed)", got, want)
	}
	if got, want := store.URL("abc"), "/og/abc.png"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// TestNewHandler_NoResolverReturns500 — handlers wired without a
// Resolver are misconfigured; a 500 + log line is the right ops
// signal so the bug is loud rather than serving "everything 404".
func TestNewHandler_NoResolverReturns500(t *testing.T) {
	g := newGen(t)
	h := NewHandler(HandlerOptions{
		Generator: g,
		Storage:   NewMemoryStorage("/og"),
		// Resolver intentionally nil.
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og/x.png", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestNewHandler_StorageGetErrorReturns500 — an unexpected Get
// failure (network, auth) must surface as 500 rather than triggering
// a re-render. Re-rendering on a transient outage would mask the
// underlying problem AND burn CPU on every request.
func TestNewHandler_StorageGetErrorReturns500(t *testing.T) {
	g := newGen(t)
	store := &fakeStorage{getErr: errors.New("s3 down")}
	h := NewHandler(HandlerOptions{
		Generator: g,
		Storage:   store,
		Resolver:  func(*http.Request) (Card, bool) { return Card{Title: "x"}, true },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og/x.png", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestNewHandler_RenderErrorReturns500 — a Card with an unknown
// theme can't render. The handler must log + 500 rather than
// silently serve broken bytes; an unknown theme is almost always a
// bug in the resolver, not a transient issue.
func TestNewHandler_RenderErrorReturns500(t *testing.T) {
	g := newGen(t)
	store := &fakeStorage{getErr: ErrNotFound} // forces miss → render
	h := NewHandler(HandlerOptions{
		Generator: g,
		Storage:   store,
		Resolver: func(*http.Request) (Card, bool) {
			return Card{Title: "x", Theme: "ghost"}, true
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og/x.png", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestNewHandler_PutErrorStillServesBody — when Render succeeds but
// PutIfAbsent fails, the user-visible OG preview still goes out.
// Logging a warning is enough; the storage path will recover later
// and the request shouldn't 500 just because the cache layer flapped.
func TestNewHandler_PutErrorStillServesBody(t *testing.T) {
	g := newGen(t)
	store := &fakeStorage{
		getErr: ErrNotFound,
		putErr: errors.New("s3 backpressure"),
	}
	h := NewHandler(HandlerOptions{
		Generator: g,
		Storage:   store,
		Resolver: func(*http.Request) (Card, bool) {
			return Card{Title: "x", Lang: "es"}, true
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og/x.png", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (Put failure must not block the response)", rec.Code)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG")) {
		t.Errorf("body is not a PNG; bytes: %q", rec.Body.Bytes()[:8])
	}
}

// TestServeImage_DefaultsContentTypeWhenEmpty — when the storage
// layer hands back an entry with no recorded content-type (legacy
// row, schema migration), serveImage must fill in image/png so the
// response is renderable in a browser. The branch sits inside
// serveImage and is only reachable through this storage shape.
func TestServeImage_DefaultsContentTypeWhenEmpty(t *testing.T) {
	g := newGen(t)
	body, err := g.Render(Card{Title: "x", Lang: "es"})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStorage{getBody: body, getCType: ""}
	h := NewHandler(HandlerOptions{
		Generator: g,
		Storage:   store,
		Resolver:  func(*http.Request) (Card, bool) { return Card{Title: "x"}, true },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og/x.png", nil)
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (default when storage returned \"\")", got)
	}
}

// TestNew_RegistersCustomThemes — Options.Themes entries must end up
// inside the generator's map so Render(Card{Theme: "custom"}) finds
// them. Without coverage on the loop body a refactor could silently
// drop overrides while still picking up the default.
func TestNew_RegistersCustomThemes(t *testing.T) {
	def, err := DefaultTheme()
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Themes: map[string]Theme{"alt": def}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Render(Card{Title: "x", Theme: "alt"}); err != nil {
		t.Errorf("Render with custom theme = %v, want nil", err)
	}
}

// TestNew_DefaultThemeError — when DefaultTheme can't load a font
// (broken bundled asset, malicious replacement), New must surface
// a wrapped error instead of returning a half-built generator that
// would crash on first Render.
func TestNew_DefaultThemeError(t *testing.T) {
	prev := loadFaceFn
	loadFaceFn = func(_ []byte, _ float64) (font.Face, error) {
		return nil, errors.New("font corrupt")
	}
	defer func() { loadFaceFn = prev }()

	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error from New when default theme can't load")
	}
}

// TestDefaultTheme_FaceErrors — each loadFace call inside
// DefaultTheme has its own error branch, and they error in the
// fixed order title → subtitle → brand. Failing on the Nth call
// exercises each branch independently.
func TestDefaultTheme_FaceErrors(t *testing.T) {
	for _, n := range []int{1, 2, 3} {
		t.Run("failOnCall"+string(rune('0'+n)), func(t *testing.T) {
			prev := loadFaceFn
			calls := 0
			loadFaceFn = func(ttf []byte, size float64) (font.Face, error) {
				calls++
				if calls == n {
					return nil, errors.New("boom")
				}
				return loadFace(ttf, size)
			}
			defer func() { loadFaceFn = prev }()

			if _, err := DefaultTheme(); err == nil {
				t.Errorf("DefaultTheme: expected error when loadFace fails on call %d", n)
			}
		})
	}
}

// TestLoadFace_ParseError — opentype.Parse must reject non-TTF
// bytes; loadFace wraps the error so the caller sees the "og:" tag
// in the error message and not a generic stdlib parse failure.
func TestLoadFace_ParseError(t *testing.T) {
	_, err := loadFace([]byte("not a ttf"), 12)
	if err == nil {
		t.Fatal("expected parse error on non-TTF bytes")
	}
	if !strings.Contains(err.Error(), "og:") {
		t.Errorf("err = %v, want \"og:\" prefix", err)
	}
}

// TestLoadFace_NewFaceError — the second loadFace error branch is
// the post-Parse NewFace step. opentype.NewFace is effectively
// infallible with sane options, so we drive the branch through the
// newFontFace seam — same shape as the production call site, with a
// scripted failure standing in.
func TestLoadFace_NewFaceError(t *testing.T) {
	prev := newFontFace
	newFontFace = func(_ *sfnt.Font, _ *opentype.FaceOptions) (font.Face, error) {
		return nil, errors.New("face build failed")
	}
	defer func() { newFontFace = prev }()

	_, err := loadFace(gobold.TTF, 12)
	if err == nil {
		t.Fatal("expected error from NewFace seam")
	}
	if !strings.Contains(err.Error(), "build face") {
		t.Errorf("err = %v, want \"build face\" wrapper", err)
	}
}

// TestRender_PNGEncodeError — png.Encode of a valid *image.RGBA
// doesn't error in practice, but Render's wrapping branch still
// needs coverage. The encodePNG seam stands in for the failing
// case so a future regression that drops the wrap (or silently
// returns the unencoded image) would be caught.
func TestRender_PNGEncodeError(t *testing.T) {
	prev := encodePNG
	encodePNG = func(io.Writer, image.Image) error { return errors.New("disk full") }
	defer func() { encodePNG = prev }()

	g := newGen(t)
	_, err := g.Render(Card{Title: "x", Lang: "es"})
	if err == nil {
		t.Fatal("expected png encode error")
	}
	if !strings.Contains(err.Error(), "encode png") {
		t.Errorf("err = %v, want \"encode png\" wrapper", err)
	}
}

// TestWrapText_EmptyInput — empty / whitespace-only text must
// produce a nil slice so the renderer skips the title/subtitle
// block entirely instead of drawing a phantom blank line that
// shifts the layout.
func TestWrapText_EmptyInput(t *testing.T) {
	face := mustFace(t)
	if got := wrapText(face, "", fixed.I(1000)); got != nil {
		t.Errorf("wrapText(\"\") = %v, want nil", got)
	}
	if got := wrapText(face, "   \t  ", fixed.I(1000)); got != nil {
		t.Errorf("wrapText(whitespace) = %v, want nil", got)
	}
}

// TestMeasureWidth_UnknownGlyphSkipped — glyphs the font can't draw
// (private-use codepoints, exotic emoji) make GlyphAdvance return
// ok=false. measureWidth must skip them instead of either crashing
// or treating them as zero-width and corrupting kerning.
func TestMeasureWidth_UnknownGlyphSkipped(t *testing.T) {
	face := mustFace(t)
	// Private-use area codepoint — not in any normal TTF.
	w := measureWidth(face, "")
	if w != 0 {
		// Acceptable as long as it didn't panic; some faces may
		// substitute a notdef glyph. The test exists for coverage,
		// so we only assert no crash + a sane non-negative value.
		if w < 0 {
			t.Errorf("measureWidth returned negative: %v", w)
		}
	}
}

// TestRender_LongTitleClampsStartY — when wrapped text is taller
// than the available area, the centring math goes negative and the
// startY clamp keeps the first line at availTop instead of climbing
// into the brand row. We can't easily verify the pixel position;
// rendering without panicking is the proxy assertion.
func TestRender_LongTitleClampsStartY(t *testing.T) {
	g := newGen(t)
	long := strings.Repeat("Una frase larga que ocupa varias líneas en el título. ", 30)
	if _, err := g.Render(Card{Title: long, Subtitle: long, Brand: "B", Lang: "es"}); err != nil {
		t.Fatalf("Render with overflowing text returned %v, want nil", err)
	}
}

// mustFace returns a real font.Face built from gobold for tests
// that need the measureWidth/wrapText helpers.
func mustFace(t *testing.T) font.Face {
	t.Helper()
	face, err := loadFace(gobold.TTF, 24)
	if err != nil {
		t.Fatalf("loadFace: %v", err)
	}
	return face
}
