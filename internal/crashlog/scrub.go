package crashlog

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxScrubError bounds one scrubbed tool-error message, in runes.
const MaxScrubError = 300

// secretValue matches credential-shaped text that a server may echo back in an
// error message. An error is free text, so it needs a value-shape scrub:
// "invalid token sk-live-..." must not become a rendered row just because it
// arrived in the wrong field.
var secretValue = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:sk|pk|ghp|gho|ghs|ghu|xox[abprs])[-_][A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode|secret|token|2fa|otp|api[_-]?key|apikey|private[_-]?key|credential|auth|bearer|cookie|session|access[_-]?key|accesskey|key)["']?\s*[:=]\s*"[^"]*"`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode|secret|token|2fa|otp|api[_-]?key|apikey|private[_-]?key|credential|auth|bearer|cookie|session|access[_-]?key|accesskey|key)["']?\s*[:=]\s*'[^']*'`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode|secret|token|2fa|otp|api[_-]?key|apikey|private[_-]?key|credential|auth|bearer|cookie|session|access[_-]?key|accesskey|key)["']?\s*[:=]\s*["']?[^"',}\s]+`),
}

// ScrubError masks credential-shaped text in an upstream error and bounds the
// result. Over-redaction is the accepted direction: a mangled message is
// cheaper than a leaked key in a row the app renders.
func ScrubError(msg string) string {
	msg = strings.TrimSpace(msg)
	for _, re := range secretValue {
		msg = re.ReplaceAllString(msg, "[redacted]")
	}
	if utf8.RuneCountInString(msg) > MaxScrubError {
		// Keep the marker INSIDE the cap and make the cut visible: a silently
		// truncated message reads as a complete one to the app.
		msg = string([]rune(msg)[:MaxScrubError-1]) + "…"
	}
	return msg
}
