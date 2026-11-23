// Package langpick negotiates a site language from an Accept-Language
// header and names languages in their own script (endonyms) for
// language switchers.
//
// Negotiate understands more than the two-letter prefix of each tag:
// an exact tag ("pt-BR") matches a supported code of the same spelling,
// and Chinese tags are mapped to the script subtags sites usually ship
// ("zh-CN", "zh-SG" → zh-Hans; "zh-TW", "zh-HK", "zh-MO" → zh-Hant),
// which a plain prefix match cannot tell apart.
package langpick

import (
	"strings"
)

// Script subtags for Chinese.
const (
	ChineseSimplified  = "zh-Hans"
	ChineseTraditional = "zh-Hant"
)

// Negotiate returns the supported code for the first Accept-Language
// tag the site can serve, or "" when none matches. Tags are taken in
// header order; quality values are ignored because browsers already
// send tags in preference order. Per tag, in order:
//
//  1. An exact, case-insensitive match ("pt-br" → "pt-BR").
//  2. For Chinese: the script variant derived from the tag
//     (Hant or a TW/HK/MO region → zh-Hant, anything else → zh-Hans),
//     then bare "zh".
//  3. The primary language subtag ("de-AT" → "de").
//
// The returned value is always spelled as in supported.
func Negotiate(acceptLanguage string, supported []string) string {
	for _, part := range strings.Split(acceptLanguage, ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || tag == "*" {
			continue
		}
		if s := lookup(supported, tag); s != "" {
			return s
		}
		primary, _, _ := strings.Cut(tag, "-")
		if primary == "zh" {
			if s := lookup(supported, chineseScript(tag)); s != "" {
				return s
			}
		}
		if s := lookup(supported, primary); s != "" {
			return s
		}
	}
	return ""
}

// chineseScript maps a lower-case Chinese tag to zh-Hans or zh-Hant.
func chineseScript(tag string) string {
	for _, sub := range strings.Split(tag, "-")[1:] {
		switch sub {
		case "hant", "tw", "hk", "mo":
			return ChineseTraditional
		case "hans":
			return ChineseSimplified
		}
	}
	return ChineseSimplified
}

// lookup returns the entry of supported equal to tag ignoring case.
func lookup(supported []string, tag string) string {
	for _, s := range supported {
		if strings.EqualFold(s, tag) {
			return s
		}
	}
	return ""
}

// endonyms are language names in their own language, keyed by the
// codes sites use in their URLs.
var endonyms = map[string]string{
	"ar": "العربية", "ca": "Català", "cs": "Čeština", "da": "Dansk",
	"de": "Deutsch", "el": "Ελληνικά", "en": "English", "es": "Español",
	"fi": "Suomi", "fr": "Français", "he": "עברית", "hi": "हिन्दी",
	"hu": "Magyar", "id": "Bahasa Indonesia", "it": "Italiano", "ja": "日本語",
	"ko": "한국어", "nb": "Norsk bokmål", "nl": "Nederlands", "pl": "Polski",
	"pt": "Português", "pt-BR": "Português (Brasil)", "ro": "Română", "ru": "Русский",
	"sv": "Svenska", "th": "ไทย", "tr": "Türkçe", "uk": "Українська",
	"vi": "Tiếng Việt", "zh": "中文", ChineseSimplified: "简体中文", ChineseTraditional: "繁體中文",
}

// Name returns the endonym for code ("de" → "Deutsch",
// "zh-Hant" → "繁體中文"), or code itself when it is not known.
func Name(code string) string {
	if n, ok := endonyms[code]; ok {
		return n
	}
	return code
}
