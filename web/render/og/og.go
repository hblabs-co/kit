package og

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// encodePNG is the seam for the final encoding step. png.Encode of a
// valid *image.RGBA is essentially infallible in stdlib, so the
// error branch is only reachable through this swap — without the
// seam the branch would be permanently uncovered.
var encodePNG = func(w io.Writer, m image.Image) error { return png.Encode(w, m) }

// Card is the content of one OG image. Two Cards with identical
// fields produce byte-identical PNGs, so Generator.Key is a stable
// content hash that doubles as the storage filename.
type Card struct {
	Title    string
	Subtitle string
	Brand    string
	Lang     string
	// Theme picks one of the named themes registered on the
	// Generator. "" (or unknown) → the "default" theme.
	Theme string
}

// Standard OG dimensions. Twitter, Slack, LinkedIn, WhatsApp all
// recommend 1200x630 — going smaller crops on retina previews,
// going larger gets downsized server-side anyway.
const (
	Width  = 1200
	Height = 630
)

// defaultThemeName is the registered name of the always-available
// theme. Card.Theme = "" routes here, and registering a Theme under
// this name overrides the built-in palette.
const defaultThemeName = "default"

// keyHashChars is the prefix length (in hex chars) of the SHA-256
// digest used as the storage key. 16 hex chars = 64 bits — large
// enough that callers won't see a collision across distinct cards
// within a deployment, small enough that filenames stay tidy.
const keyHashChars = 16

// fieldSeparator separates fields hashed by Generator.Key. NUL is
// used so two Cards with the same concatenated text but different
// splits ("ab"+"c" vs "a"+"bc") still hash differently.
const fieldSeparator byte = 0

// Options configures a Generator at boot.
type Options struct {
	// Themes registers extra themes alongside the implicit "default"
	// theme. nil → just the default. A theme set with name "default"
	// here overrides the built-in palette.
	Themes map[string]Theme
}

// Generator turns Cards into PNG bytes. Stateless w.r.t. the request
// — share one instance across goroutines.
type Generator struct {
	themes map[string]Theme
}

// New builds a Generator with the default theme always available
// under the name "default", plus any extras passed in Options.Themes.
func New(opts Options) (*Generator, error) {
	themes := map[string]Theme{}
	for k, v := range opts.Themes {
		themes[k] = v
	}
	if _, ok := themes[defaultThemeName]; !ok {
		def, err := DefaultTheme()
		if err != nil {
			return nil, fmt.Errorf("og: build default theme: %w", err)
		}
		themes[defaultThemeName] = def
	}
	return &Generator{themes: themes}, nil
}

// Render produces the PNG bytes for c. The output is deterministic
// per (Card, theme) — caller can hash the result or use Key for the
// same hash without ever rendering.
func (g *Generator) Render(c Card) ([]byte, error) {
	themeName := c.Theme
	if themeName == "" {
		themeName = defaultThemeName
	}
	theme, ok := g.themes[themeName]
	if !ok {
		return nil, fmt.Errorf("og: unknown theme %q", themeName)
	}

	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), &image.Uniform{theme.Background}, image.Point{}, draw.Src)

	if theme.AccentBar > 0 {
		barRect := image.Rect(0, Height-theme.AccentBar, Width, Height)
		draw.Draw(img, barRect, &image.Uniform{theme.AccentColor}, image.Point{}, draw.Src)
	}

	maxTextWidth := fixed.I(Width - 2*theme.PaddingX)

	if c.Brand != "" {
		brandY := theme.PaddingY + theme.BrandFace.Metrics().Ascent.Round()
		drawText(img, theme.BrandFace, theme.BrandColor, c.Brand, theme.PaddingX, brandY)
	}

	titleLines := wrapText(theme.TitleFace, c.Title, maxTextWidth)
	subLines := wrapText(theme.SubtitleFace, c.Subtitle, maxTextWidth)

	titleLineH := theme.TitleFace.Metrics().Height.Round() + theme.TitleLineGap
	subLineH := theme.SubtitleFace.Metrics().Height.Round() + theme.SubtitleLineGap

	titleBlockH := len(titleLines) * titleLineH
	subBlockH := 0
	if len(subLines) > 0 {
		subBlockH = len(subLines)*subLineH + theme.TitleSubtitleGap
	}
	totalH := titleBlockH + subBlockH

	// Vertically center the title+subtitle block in the area below
	// the brand line, leaving padding at top and the accent bar at
	// the bottom.
	availTop := theme.PaddingY*2 + theme.BrandFace.Metrics().Height.Round()
	availBottom := Height - theme.PaddingY - theme.AccentBar
	startY := availTop + (availBottom-availTop-totalH)/2
	if startY < availTop {
		startY = availTop
	}

	titleAscent := theme.TitleFace.Metrics().Ascent.Round()
	for i, line := range titleLines {
		y := startY + titleAscent + i*titleLineH
		drawText(img, theme.TitleFace, theme.TitleColor, line, theme.PaddingX, y)
	}

	if len(subLines) > 0 {
		subStart := startY + titleBlockH + theme.TitleSubtitleGap
		subAscent := theme.SubtitleFace.Metrics().Ascent.Round()
		for i, line := range subLines {
			y := subStart + subAscent + i*subLineH
			drawText(img, theme.SubtitleFace, theme.SubtitleColor, line, theme.PaddingX, y)
		}
	}

	var buf bytes.Buffer
	if err := encodePNG(&buf, img); err != nil {
		return nil, fmt.Errorf("og: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// Key returns a 16-hex-char SHA-256 prefix of the Card. Same Card →
// same Key, so it's safe to use as the storage filename and as the
// cache-buster in URLs (callers shouldn't see the hash collide
// across distinct cards within the lifetime of a deployment).
//
// Theme name is part of the hash — changing palette without
// renaming the theme silently won't invalidate cached cards.
// Bump the theme name (or keep a version suffix in the name) when
// the visual changes.
func (g *Generator) Key(c Card) string {
	h := sha256.New()
	writeField(h, c.Theme)
	writeField(h, c.Lang)
	writeField(h, c.Brand)
	writeField(h, c.Title)
	writeField(h, c.Subtitle)
	return hex.EncodeToString(h.Sum(nil))[:keyHashChars]
}

// writeField writes s plus the field separator so the boundaries
// between Card fields contribute to the hash — see fieldSeparator.
func writeField(h interface{ Write(p []byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(s))
	_, _ = h.Write([]byte{fieldSeparator})
}

// drawText writes s onto dst using face/col. (x, y) is the baseline
// of the first glyph — adjust by face.Metrics().Ascent if you want
// y to be the top of the line.
func drawText(dst *image.RGBA, face font.Face, col color.Color, s string, x, y int) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{col},
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

// wrapText breaks text into lines that fit within maxWidth using the
// face's actual glyph advances (so it tracks kerning, not just byte
// counts). Returns nil for empty input. Words longer than maxWidth
// are placed alone on a line — the result will overflow rather than
// hyphenate. Acceptable: titles are short, the theme's max chars
// per line covers normal copy.
func wrapText(face font.Face, text string, maxWidth fixed.Int26_6) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	words := strings.Fields(text)
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		candidate := current + " " + w
		if measureWidth(face, candidate) > maxWidth {
			lines = append(lines, current)
			current = w
		} else {
			current = candidate
		}
	}
	return append(lines, current)
}

// measureWidth sums advances + kerning for s in face. Mirrors
// font.Drawer.MeasureString without forcing a Drawer instance.
func measureWidth(face font.Face, s string) fixed.Int26_6 {
	var w fixed.Int26_6
	var prev rune
	first := true
	for _, r := range s {
		if !first {
			w += face.Kern(prev, r)
		}
		first = false
		adv, ok := face.GlyphAdvance(r)
		if !ok {
			continue
		}
		w += adv
		prev = r
	}
	return w
}
