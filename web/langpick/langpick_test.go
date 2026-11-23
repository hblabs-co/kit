package langpick

import "testing"

func TestNegotiate(t *testing.T) {
	both := []string{"en", "de", "pt-BR", ChineseSimplified, ChineseTraditional}
	hansOnly := []string{"en", ChineseSimplified}
	bareZh := []string{"en", "zh"}
	cases := []struct {
		header    string
		supported []string
		want      string
	}{
		{"", both, ""},
		{"*", both, ""},
		{"fr-FR,fr;q=0.9", both, ""},
		{"de-AT,de;q=0.9,en;q=0.8", both, "de"},
		{"EN-us", both, "en"},
		{"pt-br", both, "pt-BR"},
		{"pt-PT", both, ""},
		{"zh-CN,zh;q=0.9", both, ChineseSimplified},
		{"zh", both, ChineseSimplified},
		{"zh-SG", both, ChineseSimplified},
		{"zh-Hans-HK", both, ChineseSimplified},
		{"zh-TW", both, ChineseTraditional},
		{"zh-HK", both, ChineseTraditional},
		{"zh-MO", both, ChineseTraditional},
		{"zh-Hant", both, ChineseTraditional},
		{"zh-hant-cn", both, ChineseTraditional},
		{"zh-Hant ; q=0.8", both, ChineseTraditional},
		{"en-US,zh-CN", both, "en"},
		{"fr,zh-TW;q=0.5", both, ChineseTraditional},
		{"zh-TW,de", hansOnly, ""},
		{"zh-TW,en", hansOnly, "en"},
		{"zh-TW", bareZh, "zh"},
		{"zh-CN", bareZh, "zh"},
		{"fil", []string{"fi"}, ""},
		{" , de", both, "de"},
	}
	for _, c := range cases {
		if got := Negotiate(c.header, c.supported); got != c.want {
			t.Errorf("Negotiate(%q, %v) = %q, want %q", c.header, c.supported, got, c.want)
		}
	}
}

func TestName(t *testing.T) {
	cases := map[string]string{
		"de": "Deutsch", ChineseSimplified: "简体中文", ChineseTraditional: "繁體中文",
		"pt-BR": "Português (Brasil)", "xx": "xx", "": "",
	}
	for code, want := range cases {
		if got := Name(code); got != want {
			t.Errorf("Name(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestEndonymsNonEmpty(t *testing.T) {
	for code, name := range endonyms {
		if code == "" || name == "" {
			t.Errorf("empty endonym entry %q = %q", code, name)
		}
	}
}
