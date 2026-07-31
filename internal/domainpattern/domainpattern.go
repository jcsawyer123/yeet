// Package domainpattern resolves a user-supplied naming template (e.g.
// "service-{id}.dev.jcsx.me") into a concrete hostname, and enforces that
// it lands exactly one label under an allowed base domain.
//
// That one-label restriction isn't arbitrary: yeet's public ingress is a
// Caddy layer4 TLS-SNI passthrough scoped to "*.dev.jcsx.me" (see
// homelab/caddy/Caddyfile), and the wildcard cert covering it is issued
// for the same single level. A pattern that resolved two labels deep, or
// to an unrelated domain, would never get routed or certified - so this
// is rejected here, at request time, rather than failing silently later.
package domainpattern

import (
	"fmt"
	"strings"
)

const defaultPattern = "{id}"

// Resolve substitutes {id} into pattern and validates the result against
// allowedBases. An empty pattern defaults to "{id}.<first allowed base>",
// which reproduces yeet's original fixed naming scheme exactly.
func Resolve(pattern, id string, allowedBases []string) (string, error) {
	if len(allowedBases) == 0 {
		return "", fmt.Errorf("no allowed base domains configured")
	}
	if pattern == "" {
		pattern = defaultPattern + "." + allowedBases[0]
	}
	host := strings.ToLower(strings.TrimSuffix(strings.ReplaceAll(pattern, "{id}", id), "."))

	for _, base := range allowedBases {
		base = strings.ToLower(strings.TrimSpace(base))
		if base == "" {
			continue
		}
		suffix := "." + base
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		label := strings.TrimSuffix(host, suffix)
		if label == "" || strings.Contains(label, ".") {
			continue // no label, or more than one label deep under the base
		}
		return host, nil
	}
	return "", fmt.Errorf("domain %q must be exactly one label under one of the allowed base domains (%s)", host, strings.Join(allowedBases, ", "))
}
