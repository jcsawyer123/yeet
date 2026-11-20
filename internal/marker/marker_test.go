package marker

import (
	"regexp"
	"testing"
)

// coolifyDescription encodes Coolify's validation rule, from its 422:
// "The description may only contain letters (including Unicode), numbers,
// spaces, and common punctuation: - _ . , ! ? ( ) ' \" + = * / @ &".
var coolifyDescription = regexp.MustCompile(`^[\p{L}\p{N} \-_.,!?()'"+=*/@&]*$`)

func TestCoolifyRuleRejectsLegacyMarker(t *testing.T) {
	if coolifyDescription.MatchString("yeet: lulu") {
		t.Fatal("rule regexp should reject the legacy colon marker")
	}
}

func TestFormatPassesCoolifyValidation(t *testing.T) {
	for _, name := range []string{"lulu", "peakflow", "brave-otter-42", "café", "we:ird#name<>", ""} {
		d := Format(name)
		if !coolifyDescription.MatchString(d) {
			t.Errorf("Format(%q) = %q fails Coolify validation", name, d)
		}
		if !IsYeet(d) {
			t.Errorf("Format(%q) = %q not recognised by IsYeet", name, d)
		}
	}
	d := FormatInstance("my-proj", "ab12cd")
	if !coolifyDescription.MatchString(d) {
		t.Errorf("FormatInstance = %q fails Coolify validation", d)
	}
}

func TestIsYeet(t *testing.T) {
	cases := map[string]bool{
		"yeet: lulu":                     true,
		"yeet:v2 project=p instance=i":   true,
		"yeet - peakflow":                true,
		"yeet - v2 project=p instance=i": true,
		"":                               false,
		"my app":                         false,
		"yeet":                           false,
		"yeeted - foo":                   false,
		"something yeet - foo":           false,
	}
	for d, want := range cases {
		if got := IsYeet(d); got != want {
			t.Errorf("IsYeet(%q) = %v, want %v", d, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		desc, name    string
		slug, shortID string
		ok            bool
	}{
		{"yeet: lulu", "lulu", "lulu", "lulu", true},
		{"yeet: peakflow", "peakflow", "peakflow", "peakflow", true},
		{"yeet - lulu", "lulu", "lulu", "lulu", true},
		{"yeet:v2 project=shop instance=ab12", "shop-ab12", "shop", "ab12", true},
		{"yeet - v2 project=shop instance=ab12", "shop-ab12", "shop", "ab12", true},
		{FormatInstance("shop", "ab12"), "x", "shop", "ab12", true},
		{Format("lulu"), "lulu", "lulu", "lulu", true},
		{"yeet:v2 project=shop", "fallback", "fallback", "fallback", true},
		{"not ours", "x", "", "", false},
	}
	for _, c := range cases {
		slug, id, ok := Parse(c.desc, c.name)
		if slug != c.slug || id != c.shortID || ok != c.ok {
			t.Errorf("Parse(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				c.desc, c.name, slug, id, ok, c.slug, c.shortID, c.ok)
		}
	}
}
