package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// GenerateOpaqueToken returns a freshly-minted, URL-safe random token suitable
// for password resets and invitations. 32 bytes of crypto/rand = 256 bits of
// entropy, encoded to 64 hex chars.
func GenerateOpaqueToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// HashToken returns the SHA-256 hex digest of an opaque token. We store this
// in the DB (never the raw token) so a database leak can't be replayed against
// the password-reset / invitation flows.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
