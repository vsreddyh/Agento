package crashlog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Audit errors are free text from a server, so they need a value-shape scrub
// as well as the key-based redaction crash files use.
func TestScrubErrorMasksCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"invalid token sk-live-abcdef123456", "[redacted]"},
		{"Authorization: Bearer abcdefghijklmnop", "[redacted]"},
		{"api_key=supersecretvalue failed", "[redacted]"},
		{`{"token":"sk-abcdef123456"}`, "[redacted]"},
		// Compact JSON: the match must stop at the value, not swallow the
		// following fields into the replacement.
		{`{"token":"sk-abcdef123456","note":"keep"}`, "[redacted]"},
		{"passwd=hunter2 failed", "[redacted]"},
		{`token="correct horse battery staple"`, "[redacted]"},
		{"no such note: abc", "no such note: abc"},
	}
	for _, c := range cases {
		got := ScrubError(c.in)
		if c.want == "[redacted]" {
			if strings.Contains(got, "sk-live") || strings.Contains(got, "abcdefghijklmnop") ||
				strings.Contains(got, "supersecretvalue") {
				t.Fatalf("ScrubError(%q) = %q, credential survived", c.in, got)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("ScrubError(%q) = %q, want a redaction marker", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("ScrubError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := ScrubError("passwd=hunter2 failed"); strings.Contains(got, "hunter2") {
		t.Fatalf("passwd value survived: %q", got)
	}
	if got := ScrubError(`token="correct horse battery staple"`); strings.Contains(got, "correct horse") {
		t.Fatalf("quoted multi-word value survived: %q", got)
	}

	// Compact JSON: the value match must terminate, leaving the next field.
	got := ScrubError(`{"token":"sk-abcdef123456","note":"keep"}`)
	if strings.Contains(got, "sk-abcdef") || !strings.Contains(got, `"note":"keep"`) {
		t.Fatalf("compact JSON scrub = %q, want the secret masked and note kept", got)
	}

	long := strings.Repeat("x", MaxScrubError+50)
	if n := utf8.RuneCountInString(ScrubError(long)); n != MaxScrubError {
		t.Fatalf("long error scrubbed to %d runes, want %d", n, MaxScrubError)
	}
}
