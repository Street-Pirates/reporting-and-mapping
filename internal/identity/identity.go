// Package identity derives public, non-reversible user IDs from the private
// oauth2_proxy subject (email / SSO sub).
//
// The subject is NEVER persisted. Instead the datastore holds a single secret
// seed (config.user_id_hash_seed) and every user row is keyed by
//
//	opaque_id = base64url( HMAC-SHA256(key = seed, message = subject) )
//
// which is deterministic (so a returning user resolves to their existing row)
// and non-reversible without the seed. The seed must never change: rotating it
// re-derives every ID and orphans every existing user.
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// SeedConfigKey is the config table key holding the seed. It is written once, on
// first startup, and never rotated.
const SeedConfigKey = "user_id_hash_seed"

// enc is URL-safe and unpadded because opaque IDs appear in request paths
// (e.g. /api/users/{opaque}/notes).
var enc = base64.RawURLEncoding

// seedLen is the HMAC key length in bytes; 256 bits matches SHA-256's block output.
const seedLen = 32

// NewSeed returns a fresh random HMAC key, base64url-encoded for storage.
func NewSeed() (string, error) {
	b := make([]byte, seedLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate user id hash seed: %w", err)
	}
	return enc.EncodeToString(b), nil
}

// OpaqueUserID derives the public user ID for subject under encodedSeed, which
// is the value stored in config.user_id_hash_seed.
func OpaqueUserID(encodedSeed, subject string) (string, error) {
	key, err := enc.DecodeString(encodedSeed)
	if err != nil {
		return "", fmt.Errorf("decode user id hash seed: %w", err)
	}
	if len(key) == 0 {
		return "", errors.New("user id hash seed is empty")
	}
	if subject == "" {
		return "", errors.New("cannot derive an opaque id from an empty subject")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(subject))
	return enc.EncodeToString(mac.Sum(nil)), nil
}
