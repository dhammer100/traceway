package services

import (
	"crypto/rand"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// dummyBcryptHash is computed once at process start. We compare against it on
// the missing-user branch of /login and /forgot-password to equalize response
// timing — otherwise the bcrypt branch (exists) takes ~250ms while the
// short-circuit branch (does not exist) returns in microseconds, letting an
// attacker enumerate registered emails via wall-clock differences.
var dummyBcryptHash []byte

func init() {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		// Falling back to a deterministic seed defeats the timing goal weakly
		// but is preferable to panicking at init.
		seed = []byte("traceway-fallback-dummy-bcrypt-seed")
	}
	h, err := bcrypt.GenerateFromPassword(seed, bcryptCost)
	if err == nil {
		dummyBcryptHash = h
	}
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func CheckPassword(password, hash string) bool {
	if hash == "" {
		return false
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// DummyTimingWork runs a bcrypt compare against an unguessable hash. Call on
// branches that would otherwise short-circuit (e.g. user-not-found) so login /
// forgot-password handlers take roughly constant time regardless of whether
// the email matches a real account.
func DummyTimingWork() {
	if len(dummyBcryptHash) == 0 {
		return
	}
	_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte("traceway-dummy-timing-input"))
}
