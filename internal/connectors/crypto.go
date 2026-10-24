package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

// Envelope encryption for OAuth tokens (ADR-0009: field-level envelope is
// REQUIRED before the first token write — Atlas at-rest alone is not the
// cover story). AES-256-GCM with a random 96-bit nonce per value; the wire
// shape is base64url(nonce || ciphertext) — RawURLEncoding everywhere, same
// as states and PKCE values, so no decoder ever has to guess and a future
// log/debug surface can't break on +/=. (No live rows exist from before
// this encoding, so there is nothing to migrate.)
//
// The key lives in CONNECTORS_ENCRYPTION_KEY (64 hex chars = 32 bytes,
// generated with `openssl rand -hex 32`), server-side only, same secrecy as
// MONGODB_URI. Without it the OAuth flow is unconfigured and connect 501s
// (see OAuthConfigFromEnv) — tokens can never land unencrypted by forgetting
// the variable, they just can't land at all.

const encryptionKeyEnv = "CONNECTORS_ENCRYPTION_KEY"

// KeyFromEnv reads the 32-byte envelope key. An unset, malformed, or
// short key is an error, never a fallback: a weak/default key would
// silently downgrade every token row to obfuscation.
func KeyFromEnv() ([32]byte, error) {
	var zero [32]byte
	raw := strings.TrimSpace(os.Getenv(encryptionKeyEnv))
	if raw == "" {
		return zero, errors.New("connectors: " + encryptionKeyEnv + " is not set; OAuth token storage is unavailable")
	}
	// Accept bare hex only: no passwords, no passphrases, no KDF upgrade
	// path hidden in a fallback. 64 hex chars or nothing.
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != 32 {
		return zero, errors.New("connectors: " + encryptionKeyEnv + " must be 64 hex chars (32 bytes); generate with `openssl rand -hex 32`")
	}
	var key [32]byte
	copy(key[:], b)
	return key, nil
}

// EncryptToken seals one token value. A zero key refuses rather than
// encrypting under nothing — callers pass the key they loaded, and a
// missing key must fail the write, not produce a row nobody can open.
// Empty plaintext is refused too: a sealed "" would be a second
// representation of "missing" that the refresher and every reader would
// have to recognise alongside literal "". Callers pass empty through as "".
func EncryptToken(key [32]byte, plaintext string) (string, error) {
	if key == ([32]byte{}) {
		return "", errors.New("connectors: refusing to seal a token under a zero key")
	}
	if strings.TrimSpace(plaintext) == "" {
		return "", errors.New("connectors: refusing to seal an empty token; pass through empty instead")
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// DecryptToken opens one EncryptToken value. Any tamper, truncation, or
// wrong-key attempt is an error — callers treat it as "token unusable,
// reconnect", never as plaintext.
func DecryptToken(key [32]byte, sealed string) (string, error) {
	if key == ([32]byte{}) {
		return "", errors.New("connectors: refusing to open a token under a zero key")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", errors.New("connectors: token is not valid base64")
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("connectors: token is truncated")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	open, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", errors.New("connectors: token cannot be opened (wrong key or tampered)")
	}
	return string(open), nil
}
