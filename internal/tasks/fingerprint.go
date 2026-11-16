package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// FingerprintFields is every body key the create path READS, and therefore every key
// the idempotency fingerprint must cover.
//
// It is a named list rather than something derived because the consumers are spread
// across field readers and nothing enforces that a new consumed field is added here.
// That gap was real and shipped: `repeat_rule` was consumed, validated and STORED yet
// missing from the digest, so two creates differing only in custom text hashed alike.
// `TestFingerprintCoversEveryConsumedField` (cmd/health-api) walks this list and fails
// if any field stops changing the digest. When a field is added to the create path,
// add it here too — this list lives in the store package (not in one transport) so
// both the HTTP and MCP create paths share it.
var FingerprintFields = []string{
	"name", "speak_name", "description", "due_date", "due_time",
	"estimated_minutes", "parallelable",
	"repeat_every", "repeat_unit", "repeat_days", "repeat_custom", "repeat_rule",
}

// FingerprintTrimmed is the subset of FingerprintFields the create path TrimSpaces
// before storing. It must mirror that list exactly: a field trimmed by the store but
// hashed verbatim makes the digest stricter than reality and 422s a legitimate retry,
// and a field trimmed by the digest but stored verbatim makes it looser.
//
// `repeat_days` is deliberately NOT here: it is a LIST, and what the store does to it
// is canonicalisation (lowercase, de-duplicate, sort), not trimming.
var FingerprintTrimmed = map[string]bool{
	"name": true, "speak_name": true, "description": true,
	"due_date": true, "due_time": true,
	"repeat_unit": true,
	"repeat_rule": true,
}

// fingerprintZeroLike returns the zero value of whatever type a field takes elsewhere,
// so an absent key and an explicit zero hash the same. Type is taken from the OTHER
// keys present in the same body where possible; the fallbacks match what the field
// readers produce.
func fingerprintZeroLike(fields map[string]any, key string) any {
	switch key {
	case "estimated_minutes", "repeat_every":
		for _, probe := range []string{"estimated_minutes", "repeat_every"} {
			switch fields[probe].(type) {
			case float64:
				return float64(0)
			case int:
				return 0
			case json.Number:
				return json.Number("0")
			}
		}
		return float64(0)
	case "parallelable", "repeat_custom":
		for _, probe := range []string{"parallelable", "repeat_custom"} {
			if _, ok := fields[probe].(bool); ok {
				return false
			}
		}
		return false
	case "repeat_days":
		// The zero of a list is an EMPTY list, not a string: the store reads an
		// absent key and an empty array identically, so the digest must too.
		return []string{}
	default:
		return ""
	}
}

// Fingerprint digests a create body so a reused idempotency key can be told apart
// from a genuine retry. Shared by the HTTP and MCP create paths — one rule, one
// place, so the two doors cannot disagree about what "the same request" means.
//
// Derived from the DECODED body, not the raw bytes: encoding/json emits map keys in
// sorted order, so a retry with different key order or indentation still replays.
// Only consumed fields are covered, so a client adding an unknown key is not rejected.
// Absent keys hash as their zero value (the store reads absent and zero identically).
// Trimmed exactly where the create path trims; `speak_name` truncated like the store
// (120 bytes); `repeat_days` canonicalised like the store.
func Fingerprint(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	trimmed := FingerprintTrimmed
	relevant := make(map[string]any, len(FingerprintFields))
	for _, k := range FingerprintFields {
		v, ok := fields[k]
		if !ok {
			relevant[k] = fingerprintZeroLike(fields, k)
			continue
		}
		if trimmed[k] {
			if str, isStr := v.(string); isStr {
				v = strings.TrimSpace(str)
			}
		}
		if k == "speak_name" {
			if str, isStr := v.(string); isStr {
				v = TruncateBytes(str, 120)
			}
		}
		if k == "repeat_days" {
			if days, ok := ParseRepeatDays(v); ok {
				if days == nil {
					days = []string{}
				}
				v = days
			}
		}
		relevant[k] = v
	}
	if len(relevant) == 0 {
		return ""
	}
	raw, err := json.Marshal(relevant)
	if err != nil {
		// Unencodable means the body holds a type the create will reject anyway.
		// Returning "" disables the mismatch check rather than failing the write.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
