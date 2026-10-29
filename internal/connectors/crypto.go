package connectors

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"unicode/utf8"
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
// the variable, they just can't land at all. There is no key versioning:
// rotating the key invalidates every stored grant (old seals stop opening),
// so every connector needs reconnect after a rotation — not a provider
// outage, an expected re-consent.

const encryptionKeyEnv = "CONNECTORS_ENCRYPTION_KEY"

// KeyFromEnv reads the 32-byte envelope key. An unset, malformed, or
// short key is an error, never a fallback: a weak/default key would
// silently downgrade every token row to obfuscation.
func KeyFromEnv() ([32]byte, error) {
	var zero [32]byte
	raw := TrimCredential(os.Getenv(encryptionKeyEnv))
	if raw == "" {
		return zero, errors.New("connectors: " + encryptionKeyEnv + " is not set; OAuth token storage is unavailable")
	}
	// Same fail-fast vocabulary as the client-credential gates: an
	// operator-pasted space/control fails here with the specific
	// discriminant instead of the generic 64-hex-chars message the
	// decode below would produce for the same input class.
	if !validCredential(raw) {
		return zero, errors.New("connectors: " + encryptionKeyEnv + " must not contain control characters, spaces, or invalid UTF-8")
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
	if TrimCredential(plaintext) == "" {
		return "", errors.New("connectors: refusing to seal an empty token; pass through empty instead")
	}
	// Length before padded/controls (empty → length → padded → control):
	// the same input pair reports the same discriminant on every leg.
	// Capped before the alloc: gcm.Seal allocates the full output up
	// front, so an unbounded plaintext buys a large alloc before the
	// store 400s the resulting cipher. Plaintext grants stay well under
	// MaxPlaintextLen in prod — a legitimate token never trips this.
	if len(plaintext) > MaxPlaintextLen {
		return "", errors.New("connectors: refusing to seal an oversized token")
	}
	if plaintext != TrimCredential(plaintext) {
		// Refuse padded plaintext instead of sealing it: a sealed-with-
		// padding value is valid base64url with no spaces, so the
		// SetTokens padded-cipher gate cannot see it — but it decrypts
		// to a value Google rejects. Checked == stored, all the way
		// down (same contract as the caller-side trim-before-seal).
		return "", errors.New("connectors: refusing to seal a padded token; trim before sealing")
	}
	// Controls, invalid UTF-8, and whitespace fail here like every sibling
	// input: a sealed-with-space plaintext is valid base64url the SetTokens
	// gates cannot see, but decrypts to a value Google rejects. Same
	// HasWhitespace gate as the state/verifier/code legs (interior-only by
	// construction — seal callers trim edges first).
	if HasControl(plaintext) || !utf8.ValidString(plaintext) || HasWhitespace(plaintext) {
		return "", errors.New("connectors: refusing to seal an invalid token")
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
//
// Do not call with "": it errors ("truncated"), by design — "" is the
// documented missing-token sentinel and must be checked by the caller
// (if tok == "" → renew) before decrypting. An early "" → "" return
// would collapse "missing" into "decrypted to empty" and hide the branch
// the refresher keys off.
//
// Length is bounded here (same MaxCipherLen as the write path):
// this is the read-side enforcement point — GetTokens returns stored
// rows verbatim, so an oversized artifact from before the cap (or a
// hand-edit) fails here instead of becoming a large alloc downstream.
// A multi-MB cipher is never a real grant; it is garbage the store
// must not serve.
func DecryptToken(key [32]byte, sealed string) (string, error) {
	if key == ([32]byte{}) {
		return "", errors.New("connectors: refusing to open a token under a zero key")
	}
	// Bounded before decoding: base64 decode allocates the full output
	// up front, so a multi-MB hand-edited column would buy a large alloc
	// before the nonce-length check ever runs. Same MaxCipherLen the
	// write path enforces (sealed tokens are a few hundred chars), so a
	// legitimate grant never trips it.
	if len(sealed) > MaxCipherLen {
		return "", errors.New("connectors: token is too long")
	}
	// Whitespace/padded input fails here with its own discriminant, like
	// the write-side padded gate: "" reads as truncated (missing), but a
	// blank or padded cipher is the "present but unopenable" shape the
	// refresher must never mistake for a grant.
	if sealed != "" && TrimCredential(sealed) == "" {
		return "", errors.New("connectors: refusing to open a blank or padded token; trim before opening")
	}
	if sealed != TrimCredential(sealed) {
		return "", errors.New("connectors: refusing to open a padded token; trim before opening")
	}
	// Interior whitespace fails here with its own discriminant, matching
	// the write-side HasWhitespace gate: "ab cd" would otherwise fall
	// through to the generic base64 error below. Edges belong to the
	// blank/padded gates above; only interior whitespace reaches here.
	if HasWhitespace(sealed) {
		return "", errors.New("connectors: token contains whitespace")
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
