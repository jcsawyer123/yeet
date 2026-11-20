// Package marker owns the ownership tag yeet writes into each Coolify
// resource's description, so yeet can tell its own resources apart from
// everything else on the server.
//
// Coolify validates descriptions and only allows letters (including
// Unicode), numbers, spaces and - _ . , ! ? ( ) ' " + = * / @ &. The
// original "yeet: <name>" marker uses a colon and is now rejected on
// create, so new resources are written as "yeet - <name>". Resources
// created before the change still carry "yeet: <name>" (and possibly the
// "yeet:v2 project=<slug> instance=<id>" form), so both prefixes are
// recognised when reading.
package marker

import (
	"strings"
	"unicode"
)

const (
	// Prefix is written on every new resource.
	Prefix = "yeet - "
	// LegacyPrefix is what resources created before Coolify's description
	// validation carry. Read-only: never written any more.
	LegacyPrefix = "yeet:"
)

// Format returns the description for a new yeet-owned resource. Any
// character Coolify would reject is replaced with '-', so a free-form
// name can never make a create fail validation.
func Format(name string) string {
	return Prefix + sanitize(name)
}

// FormatInstance returns the v2 description carrying an explicit project
// slug and instance short id, in the Coolify-safe form
// "yeet - v2 project=<slug> instance=<id>".
func FormatInstance(slug, shortID string) string {
	return Prefix + "v2 project=" + sanitize(slug) + " instance=" + sanitize(shortID)
}

// IsYeet reports whether a description marks a resource as yeet-owned,
// in either the current or the legacy format.
func IsYeet(description string) bool {
	return strings.HasPrefix(description, Prefix) || strings.HasPrefix(description, LegacyPrefix)
}

// Parse extracts the project slug and instance short id from a yeet
// marker. A v2 marker ("yeet:v2 project=<slug> instance=<id>" or
// "yeet - v2 project=<slug> instance=<id>") yields its explicit fields;
// anything else is a plain "<prefix><name>" resource, adopted as its own
// single-instance project keyed by the Coolify resource name. ok is false
// when the description is not a yeet marker at all.
func Parse(description, name string) (slug, shortID string, ok bool) {
	rest, found := strings.CutPrefix(description, Prefix)
	if !found {
		rest, found = strings.CutPrefix(description, LegacyPrefix)
	}
	if !found {
		return "", "", false
	}
	if v2, isV2 := strings.CutPrefix(strings.TrimLeft(rest, " "), "v2 "); isV2 {
		var project, instance string
		for _, f := range strings.Fields(v2) {
			if v, ok := strings.CutPrefix(f, "project="); ok {
				project = v
			}
			if v, ok := strings.CutPrefix(f, "instance="); ok {
				instance = v
			}
		}
		if project != "" && instance != "" {
			return project, instance, true
		}
	}
	return name, name, true
}

// allowedPunct is the punctuation Coolify accepts in descriptions.
const allowedPunct = `-_.,!?()'"+=*/@&`

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == ' ' || strings.ContainsRune(allowedPunct, r) {
			return r
		}
		return '-'
	}, s)
}
