package siteinfo

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// TestInfo_EmbeddedIsPopulated guards against a bad edit / empty paste
// in siteinfo.yaml: the embedded doc must have a legal entity and at
// least one fully-specified social link. Mirrors the intent of
// uikit's TestIconSet_AllEntriesNonEmpty and the projects tests —
// catch a structural mistake at CI, not as a blank footer in prod.
func TestInfo_EmbeddedIsPopulated(t *testing.T) {
	if strings.TrimSpace(Info.LegalEntity) == "" {
		t.Error("Info.LegalEntity is empty — footer copyright would render blank")
	}
	if strings.TrimSpace(Info.LegalAddress) == "" {
		t.Error("Info.LegalAddress is empty — privacy page would render blank address")
	}
	if strings.TrimSpace(Info.ContactEmail) == "" {
		t.Error("Info.ContactEmail is empty — mailto: CTAs would be broken")
	}
	if strings.TrimSpace(Info.PrivacyEmail) == "" {
		t.Error("Info.PrivacyEmail is empty — privacy page DPO mailto: would be broken")
	}
	if len(Info.Social) == 0 {
		t.Fatal("Info.Social is empty — footer social column would render nothing")
	}
	for i, s := range Info.Social {
		if strings.TrimSpace(s.Label) == "" {
			t.Errorf("Info.Social[%d] has empty Label", i)
		}
		if strings.TrimSpace(s.URL) == "" {
			t.Errorf("Info.Social[%d] (%q) has empty URL", i, s.Label)
		}
	}
}

// TestLoad_OK exercises the happy path against the real embedded FS
// (the same call package init makes) and asserts the parsed shape.
func TestLoad_OK(t *testing.T) {
	got, err := load(infoFS, infoPath)
	if err != nil {
		t.Fatalf("load: unexpected error: %v", err)
	}
	if got.LegalEntity == "" || len(got.Social) == 0 {
		t.Fatalf("load: parsed empty SiteInfo: %+v", got)
	}
}

// TestLoad_ReadError covers the fs.ReadFile failure branch: the path
// is absent from the filesystem.
func TestLoad_ReadError(t *testing.T) {
	_, err := load(fstest.MapFS{}, "missing.yaml")
	if err == nil || !strings.Contains(err.Error(), "read missing.yaml") {
		t.Fatalf("want read error, got %v", err)
	}
}

// TestLoad_ParseError covers the strict-decode failure branch: an
// unknown key must fail because the loader sets KnownFields(true).
func TestLoad_ParseError(t *testing.T) {
	fsys := fstest.MapFS{
		"bad.yaml": {Data: []byte("legal_entity: x\nbogus_key: y\n")},
	}
	_, err := load(fsys, "bad.yaml")
	if err == nil || !strings.Contains(err.Error(), "parse bad.yaml") {
		t.Fatalf("want parse error, got %v", err)
	}
}

// TestMustLoad_PanicsOnLoadError drives the panic branch via the
// loadFn seam — the only way to reach it, since the real embedded
// YAML is never missing or malformed.
func TestMustLoad_PanicsOnLoadError(t *testing.T) {
	orig := loadFn
	loadFn = func(fs.FS, string) (SiteInfo, error) {
		return SiteInfo{}, errors.New("boom")
	}
	defer func() {
		loadFn = orig
		if r := recover(); r == nil {
			t.Fatal("mustLoad did not panic on load error")
		}
	}()
	mustLoad()
}

// TestFuncMap asserts both helpers return the embedded data and that
// socialLinks hands back an independent copy (mutating it must not
// corrupt the shared Info.Social).
func TestFuncMap(t *testing.T) {
	fm := FuncMap()

	le, ok := fm["legalEntity"].(func() string)
	if !ok {
		t.Fatal("legalEntity helper missing or wrong signature")
	}
	if got := le(); got != Info.LegalEntity {
		t.Errorf("legalEntity() = %q, want %q", got, Info.LegalEntity)
	}

	la, ok := fm["legalAddress"].(func() string)
	if !ok {
		t.Fatal("legalAddress helper missing or wrong signature")
	}
	if got := la(); got != Info.LegalAddress {
		t.Errorf("legalAddress() = %q, want %q", got, Info.LegalAddress)
	}

	ce, ok := fm["contactEmail"].(func() string)
	if !ok {
		t.Fatal("contactEmail helper missing or wrong signature")
	}
	if got := ce(); got != Info.ContactEmail {
		t.Errorf("contactEmail() = %q, want %q", got, Info.ContactEmail)
	}

	pe, ok := fm["privacyEmail"].(func() string)
	if !ok {
		t.Fatal("privacyEmail helper missing or wrong signature")
	}
	if got := pe(); got != Info.PrivacyEmail {
		t.Errorf("privacyEmail() = %q, want %q", got, Info.PrivacyEmail)
	}

	sl, ok := fm["socialLinks"].(func() []SocialLink)
	if !ok {
		t.Fatal("socialLinks helper missing or wrong signature")
	}
	got := sl()
	if !reflect.DeepEqual(got, Info.Social) {
		t.Errorf("socialLinks() = %+v, want %+v", got, Info.Social)
	}
	// Copy semantics: the returned slice must not alias Info.Social.
	if len(got) > 0 {
		got[0].Label = "MUTATED"
		if Info.Social[0].Label == "MUTATED" {
			t.Error("socialLinks() leaked the shared backing array")
		}
	}
}
