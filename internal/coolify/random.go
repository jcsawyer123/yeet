package coolify

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomName returns a short, URL-safe, unnamed-on-purpose identifier
// like "yeet-a3f9c1" for deployments the user didn't bother naming.
func RandomName() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "yeet-" + hex.EncodeToString(b)
}
