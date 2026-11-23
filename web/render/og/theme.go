package og

import (
	"fmt"
	"image/color"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
)

// Theme is the visual style of an OG card. Values are concrete (not
// pointers) so a Theme is safe to copy into the Generator's map; the
// font.Faces inside are also goroutine-safe by design.
//
// Sizes are integer pixel offsets so the renderer can stay in
// integer math throughout. PaddingX/PaddingY are uniform inner
// margins; AccentBar is the height of the bottom band (set to 0 to
// disable).
type Theme struct {
	Background    color.Color
	BrandColor    color.Color
	TitleColor    color.Color
	SubtitleColor color.Color
	AccentColor   color.Color

	BrandFace    font.Face
	TitleFace    font.Face
	SubtitleFace font.Face

	PaddingX int
	PaddingY int

	TitleLineGap     int // extra space between wrapped title lines
	SubtitleLineGap  int // extra space between wrapped subtitle lines
	TitleSubtitleGap int // gap between the title block and the subtitle block

	AccentBar int // bottom accent strip height; 0 = none
}

// loadFaceFn is the seam tests swap to drive DefaultTheme's error
// branches deterministically. Production always uses the real
// loadFace; tests can replace it with a counter-based fake that
// fails on the Nth call to exercise each error path independently.
var loadFaceFn = loadFace

// Font sizes (in points) and layout offsets (in pixels) for the
// built-in default theme. Pulled out so each value is named once
// and DefaultTheme reads as a configuration, not a wall of
// integers. Tweak when the design system lands.
const (
	defaultTitleFontSize    = 76
	defaultSubtitleFontSize = 36
	defaultBrandFontSize    = 28

	defaultPaddingX         = 80
	defaultPaddingY         = 70
	defaultTitleLineGap     = 6
	defaultSubtitleLineGap  = 4
	defaultTitleSubtitleGap = 24
	defaultAccentBar        = 12
)

// fontDPI is the dots-per-inch the renderer assumes when converting
// point sizes to pixels. 72 is the typographic standard ("1 point =
// 1 pixel at 72 DPI") and the de-facto default in golang.org/x/image.
const fontDPI = 72

// DefaultTheme returns the built-in palette: deep navy background,
// warm-orange brand mark, white title, muted-grey subtitle, orange
// accent strip at the bottom. Uses the Go fonts (gobold + goregular)
// so the binary stays self-contained — no font asset to embed.
//
// Tweak the colours and font sizes here when the design system lands;
// services that want a different look register their own Theme via
// Options.Themes and pass the name through Card.Theme.
func DefaultTheme() (Theme, error) {
	title, err := loadFaceFn(gobold.TTF, defaultTitleFontSize)
	if err != nil {
		return Theme{}, err
	}
	subtitle, err := loadFaceFn(goregular.TTF, defaultSubtitleFontSize)
	if err != nil {
		return Theme{}, err
	}
	brand, err := loadFaceFn(gobold.TTF, defaultBrandFontSize)
	if err != nil {
		return Theme{}, err
	}
	return Theme{
		Background:       color.RGBA{0x0a, 0x0e, 0x1a, 0xff},
		BrandColor:       color.RGBA{0xff, 0xa6, 0x39, 0xff},
		TitleColor:       color.RGBA{0xff, 0xff, 0xff, 0xff},
		SubtitleColor:    color.RGBA{0xc6, 0xcd, 0xd9, 0xff},
		AccentColor:      color.RGBA{0xff, 0xa6, 0x39, 0xff},
		BrandFace:        brand,
		TitleFace:        title,
		SubtitleFace:     subtitle,
		PaddingX:         defaultPaddingX,
		PaddingY:         defaultPaddingY,
		TitleLineGap:     defaultTitleLineGap,
		SubtitleLineGap:  defaultSubtitleLineGap,
		TitleSubtitleGap: defaultTitleSubtitleGap,
		AccentBar:        defaultAccentBar,
	}, nil
}

// newFontFace is the seam for the second-stage face construction so
// tests can drive the failure branch of loadFace without crafting a
// malformed font that would happen to fail opentype.NewFace.
var newFontFace = opentype.NewFace

// loadFace parses a TTF byte slice and builds a font.Face at the
// requested point size. Hinting is set to Full so glyph edges stay
// crisp at the modest sizes we use.
func loadFace(ttf []byte, size float64) (font.Face, error) {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil, fmt.Errorf("og: parse ttf: %w", err)
	}
	face, err := newFontFace(f, &opentype.FaceOptions{
		Size:    size,
		DPI:     fontDPI,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("og: build face: %w", err)
	}
	return face, nil
}
