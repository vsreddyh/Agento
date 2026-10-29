package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agento/internal/approvals"

	"golang.org/x/oauth2"
)

// OAuth exchange for the first connectors (#316, second layer).
//
// v1 foundation answered POST .../connect with 501. This layer wires the
// real Google flow: the app opens the auth URL in a browser, Google
// redirects to the server callback with a code, the server exchanges it and
// stores the ENCRYPTED tokens (see crypto.go), and the connector flips to
// connected. The app polls GET /api/connectors; it never sees a code, a
// verifier, or a token anywhere in the flow.

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"

	// StateTTL bounds one connect attempt: the user has ten minutes from
	// tapping connect to finishing the Google consent screen.
	StateTTL = 10 * time.Minute

	// MaxAccessLifetime bounds a stored access-token expiry: provider
	// expires_in is ~1h, so anything past a day is skew, a stubby test
	// token, or a hand-edit — all of which must renew soon, not never.
	// See TokenRow for the stored-side contract; enforced at the gateway
	// write site and in both SetTokens.
	MaxAccessLifetime = 24 * time.Hour

	// StatesCollection holds pending connect attempts. Short-lived by TTL;
	// consumed (deleted) on first use, so a code can never be exchanged twice.

	StatesCollection = "oauth_states"
)

// ClampAccessExpiry bounds an access-token expiry timestamp against now:
// anything past now+MaxAccessLifetime reads as renew-soon, never as
// don't-renew-for-decades. Single owner for the bound: the gateway write
// site, both SetTokens, and the Exchange return all call this instead of
// inlining now.Add(MaxAccessLifetime) — and any future refresher must
// call it on read, since GetTokens serves hand-edited futures verbatim
// (see TokenRow). Wall-clock by design; callers with an injectable clock
// pass their own now.
// EffectiveExpiry is the read-side bound every refresher must apply: the
// stored value with now clamped to MaxAccessLifetime, so a hand-edited
// far-future row reads as renew-soon instead of don't-renew-for-decades.
// Testable without a store (pure function of two ints); GetTokens stays
// verbatim by design, and this is the single owner callers use instead.
// Negatives floor to 0 while positive past timestamps return as-is: both
// mean renew-now per TokenRow, but only negatives lack any other spelling —
// a past timestamp stays addressable (when), a negative becomes the
// sentinel (missing). Renew-now check is therefore `expiry <= now`, never
// `== 0`: branching on the sentinel alone would treat a past timestamp
// as still valid.
func (t TokenRow) EffectiveExpiry(now time.Time) int64 {
	return ClampAccessExpiry(t.AccessExpiryUnixMilli, now)
}

func ClampAccessExpiry(expiryUnixMilli int64, now time.Time) int64 {
	// Floor negatives to zero first: any past timestamp renews immediately
	// per TokenRow, so a negative (clock-skew stub, hand-edit) is past, not
	// a third missing representation — one rule for both signs. Zero is the
	// single spelling of "already expired".
	if expiryUnixMilli < 0 {
		return 0
	}
	if max := now.Add(MaxAccessLifetime).UnixMilli(); expiryUnixMilli > max {
		return max
	}
	return expiryUnixMilli
}

// GoogleScopes maps a connector to the Google OAuth scopes its auth URL
// requests. Least privilege per connector, and deliberately NOT the
// full-access variants:
//
//   - gmail: readonly + compose. compose creates/updates/deletes DRAFTS
//     only — it cannot send. The exact scope the draft-only rule needs:
//     the token is incapable of sending even if the relay check vanished.
//   - calendar: readonly. Draft events are agent-side objects (created
//     locally, sent nowhere) until the approval-gate flow covers writes,
//     so the grant carries no write bit at all.
//
// Follow-up sync obligation (same class as IsDraftOnlySend): if a wired MCP
// server needs a broader scope, BOTH this table and the tool policy move
// together, and the ADR records why. Folds like every store verb: same
// logical id, same answer.
func GoogleScopes(id string) []string {
	if len(id) > MaxConnectorIDLen {
		return nil
	}
	id = NormalizedID(id)
	switch id {
	case Gmail:
		return []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.compose",
		}
	case Calendar:
		return []string{
			"https://www.googleapis.com/auth/calendar.readonly",
		}
	default:
		return nil
	}
}

// OAuthConfig is the server-side OAuth client. Nil *OAuthConfig (the
// gateway's ConnectorOAuth field) means unconfigured: connect 501s exactly
// like the v1 foundation, and no token can be stored.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Key          [32]byte
	// Endpoint override for tests (httptest token server). Production always
	// uses Google's endpoints below.
	AuthURL  string
	TokenURL string
}

// isDigits reports a non-empty all-ASCII-digit string: SplitHostPort does
// not check numeracy, and strconv.Atoi accepts "+80"/leading-zero forms
// that JoinHostPort would re-emit verbatim (validated != sent). The gate
// below canonicalises leading-zero spellings ("080" sends as "80") and
// rejects the +/- spellings Atoi itself accepts — isDigits is what kills
// the latter, not the former.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parsePort validates a SplitHostPort port string and returns its numeric
// value for re-emit: strict digits-only, range-checked 1-65535. One helper
// so isDigits + Atoi + range can never be separated by a "simplify to
// Atoi-only" edit that re-opens the +/- spellings Atoi accepts or the
// verbatim-emit trap (leading zeros canonicalise via Itoa on return;
// "+"/"-"/space never validate).
func parsePort(p string) (int, bool) {
	if !isDigits(p) {
		return 0, false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// validRedirectHost reports whether host is a registrable spelling:
// ASCII labels of [a-z0-9-] with no empties and no leading/trailing
// hyphens, at least one dot, a lettered TLD of length 2+, and no
// RFC-2606 reserved TLD. "localhost" and IP literals bypass the rules
// (loopback-ness is checked separately). The console registration is
// punycode, never Unicode. Registrability — not just syntax — is the
// bar: an unregistrable host passes every other check and dies with
// redirect_uri_mismatch in the browser, so it must fail here.
// Takes the already-folded host: CanonicalRedirectURL lowercases before
// calling, so checked == sent by construction and this predicate never
// sees the raw spelling.
func validRedirectHost(host string) bool {
	// Byte lengths throughout (253/63): valid because the ASCII gate below
	// rejects c >= 0x80, making bytes == runes on every accepted input.
	// Ordering is deliberate only in outcome — the gates reject the same
	// set either way — so a future Unicode widening must fix the units
	// first. Discriminant note: a 200-rune Unicode host (~600 bytes) trips
	// the length gate before ASCII names the real cause — triage a length
	// reject on a long host by checking for non-ASCII (register punycode)
	// first.
	if host == "" || len(host) > 253 {
		return false
	}
	if host == "localhost" {
		return true
	}
	if !strings.Contains(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if c >= 0x80 || !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	// The TLD itself must be all letters, multi-char: ICANN never issues
	// all-numeric, single-char, or digit-mixed TLDs, so "myhost.123",
	// "a.b", and "myhost.c0m" are typos that would die at Google instead
	// of here. A letter elsewhere is not enough ("123.com" stays valid).
	// The "xn--" punycode prefix is the one legitimate mixer: its suffix
	// is encoded Unicode, validated by the label rules above.
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	if strings.HasPrefix(tld, "xn--") {
		if len(tld) == 4 {
			return false
		}
	} else {
		for i := 0; i < len(tld); i++ {
			if c := tld[i]; c < 'a' || c > 'z' {
				return false
			}
		}
	}
	// Reserved + private-use names can never be public registrations (and
	// the http loopback gate only whitelists bare "localhost"):
	// validated-shape would never match a console entry. RFC-2606
	// (.test, .example, .invalid), mDNS (.local), unqualified-LAN (.lan,
	// .internal, .home, .corp, .localdomain, .intranet), special-use
	// (.onion, .arpa incl. .home.arpa) all validate syntactically yet no
	// public Google registration can hold them — same validated-shape !=
	// registrable-shape class as the single-char/numeric-TLD gates above.
	// Best-effort, not exhaustive: an unlisted private TLD still fails at
	// Google, not here — the list names the common cases so triage reads
	// the common typos fast. Fail here instead of dying with
	// redirect_uri_mismatch in the browser.
	switch tld {
	case "test", "example", "invalid", "local", "internal", "lan", "home", "corp", "localdomain", "intranet", "onion", "arpa":
		return false
	}
	// Bare "localhost" only, in both directions: "*.localhost" resolves
	// loopback locally but no public registration can hold it, and
	// "localhost.<anything>" is just as unregistrable (same class as the
	// TLD rejects above). The http gate whitelists bare "localhost" alone —
	// accepting either form would validate spellings nothing else in the
	// pipeline accepts. Local-CA subdomain setups stay out of scope.
	if strings.HasSuffix(host, ".localhost") || strings.HasPrefix(host, "localhost.") {
		return false
	}
	return true
}

// RedactURL strips any userinfo for logging: a hand-typed redirect with
// credentials must never land verbatim in a container log, even on the
// rejection path that exists to keep credentials out of the log in the
// first place. Unparseable input is scrubbed + bounded inside the helper,
// not left to the call site's verb: it failed Parse, so no structured
// userinfo could be extracted — but the raw bytes are still
// caller-influenced, so the helper strips controls, scrubs, and bounds,
// never returning the raw string. What remains is the operator's own
// typo, needed for triage.
// stripUserinfo best-effort removes a userinfo prefix from a value url.Parse
// rejected: userinfo (if any) ends at the last "@" before the first "/",
// with or without a scheme (a schemeless hand-typed "user:pass@host/..."
// fails Parse differently but carries the same credentials). s must already
// be control-stripped. Returns s unchanged when no "@"-before-"/" exists.
func stripUserinfo(s string) string {
	// Find the authority section: after "://" if present, else the whole
	// string up to the first "/".
	search := s
	prefix := ""
	if i := strings.Index(s, "://"); i >= 0 {
		prefix = s[:i+3]
		search = s[i+3:]
	}
	seg := search
	tail := ""
	if slash := strings.Index(search, "/"); slash >= 0 {
		seg = search[:slash]
		tail = search[slash:]
	}
	if at := strings.LastIndex(seg, "@"); at >= 0 {
		return prefix + seg[at+1:] + tail
	}
	return s
}

func RedactURL(raw string) string {
	// Same trim as every sibling gate (TrimCredential, not TrimSpace): an
	// edge Cf forgiven by CanonicalRedirectURL must not survive here, so
	// checked == logged by construction. Interior strip matches HasControl
	// (Cc plus Cf), for the same reason. Length-capped before the
	// rune-map + Parse below, matching CanonicalRedirectURL: a multi-MB
	// misconfigured value must not pay map + parse on every tap.
	trimmed := TrimCredential(raw)
	// Byte cap with a rune-boundary cut (same walk as truncateForLog):
	// never slice through a multi-byte char. The cut bounds the map +
	// parse work below; the marker is appended after ScrubError (which
	// bounds to 300 runes and would otherwise swallow it), so the startup
	// line stays honest about the truncation.
	cut := len(trimmed) > MaxRedirectLen
	work := trimmed
	if cut {
		end := 0
		for i, r := range work {
			if w := utf8.RuneLen(r); i+w > MaxRedirectLen {
				break
			} else {
				end = i + w
			}
		}
		work = work[:end]
	}
	mark := func(s string) string {
		// Marker exempt from the bound by design: it rides past ScrubError's
		// 300-rune cap (appended after, never swallowed) so the startup line
		// stays honest. Log-only overage (~15 runes); the parse/map work
		// above is what the cap actually bounds, and every call site caps
		// again (truncateForLog/bounded).
		if cut {
			return s + "\u2026[truncated]"
		}
		return s
	}
	strip := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, s)
	}
	parsed, err := url.Parse(work)
	if err != nil {
		return mark(approvals.ScrubError(stripUserinfo(strip(work))))
	}
	if parsed.User == nil {
		// No userinfo to strip, but the value is still operator-influenced:
		// strip controls and scrub+bound so the helper is safe under any
		// verb, not just the %q call sites use today. Clean short URLs
		// pass through unchanged (nothing to scrub, under the bound).
		return mark(approvals.ScrubError(strip(work)))
	}
	parsed.User = nil
	// Scrubbed + bounded like the other branches: the stripped form is
	// still operator-influenced (hand-typed host/path), so the helper
	// never returns a raw string on any branch. Clean short URLs pass
	// through unchanged (nothing to scrub, under the bound).
	return mark(approvals.ScrubError(parsed.String()))
}

// CanonicalRedirectURL validates and canonicalises in one step so every
// caller sends the exact registered shape: absolute http(s), https unless
// loopback, no userinfo/query/fragment, path ending in the single
// registered callback endpoint. path.Clean collapses double slashes and
// dot segments before the suffix check, so validated-shape == sent-shape
// by construction — Google exact-matches redirect_uri, and a mismatch
// dies in the browser with nothing in the server log pointing at it.
func CanonicalRedirectURL(raw string) (string, error) {
	// Raw length first: TrimCredential walks the whole input, so an
	// unbounded hand-built value pays the walk before any gate. Same bound
	// and error as the trimmed gate below — trimming only shrinks, so this
	// fires first on the same inputs (plus pathological padding, same 400).
	if len(raw) > MaxRedirectLen {
		return "", errors.New("connectors: redirect URL too long")
	}
	// Trimmed once, parsed once: every check below sees the same string,
	// so a future edit cannot split checked-vs-parsed by re-deriving one.
	trimmed := TrimCredential(raw)
	// Length-capped before Parse: a multi-MB hand-built value would
	// otherwise pay the full parse + Clean + String alloc before any
	// 400. Same MaxRedirectLen the CreateState redirect cap uses —
	// registrations are ~60 chars, so 2048 is generous, not tight.
	if len(trimmed) > MaxRedirectLen {
		return "", errors.New("connectors: redirect URL too long")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("connectors: redirect URL must be an absolute http(s) URL")
	}
	// Inner controls fail closed like every sibling input (state/code/
	// verifier at every layer): url.Parse/String round-trips them, so a
	// control-bearing path would pass validation yet mismatch Google.
	// Checked on the trimmed form — the same string Parse saw — so
	// edge whitespace (forgiven by the trim contract) doesn't misfile
	// as a control-character error.
	// Hostnames are case-insensitive: fold before validation so a
	// mixed-case registration compares equal to what Google
	// exact-matches against (ports are unaffected by lowercasing).
	// A trailing-dot FQDN spelling is folded too: the console
	// registration is canonically dot-less, and the dot would
	// otherwise pass validation yet mismatch Google exactly.
	// TrimRight, not TrimSuffix: "host.." must fold fully, not to "host.".
	// (Fold before the empty check: a bare "." host must fail as empty,
	// not validate as dot-less nothing.)
	parsed.Host = strings.TrimRight(strings.ToLower(parsed.Host), ".")
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("connectors: redirect URL must be an absolute http(s) URL")
	}
	if strings.Contains(parsed.Host, "%") {
		// No legitimate registration percent-encodes the host (zone IDs
		// included — a link-local callback is not a real registration):
		// %00/%2e spellings would otherwise pass every check verbatim
		// yet mismatch Google exactly.
		return "", errors.New("connectors: redirect URL host must not contain percent-encoding")
	}
	// Strip default ports: the console registration is canonically
	// port-less, while an operator-supplied URL may name :443/:80
	// explicitly — and Google exact-matches redirect_uri. The hostname
	// is re-folded after the split too: "host.:8443" must canonicalise
	// to "host:8443", not pass validation dotted. Brackets go back on
	// IPv6 literals, or the stripped host stops parsing as a URL.
	if h, p, err := net.SplitHostPort(parsed.Host); err == nil {
		h = strings.TrimRight(h, ".")
		switch {
		case p == "":
			// Bare host only — no JoinHostPort, nothing to re-emit. A
			// dangling colon ("host:") splits cleanly with an empty port,
			// but no registration can hold it: reject like the
			// non-numeric branch instead of canonicalising the colon
			// away (validated != sent, same class as every other gate).
			if strings.Contains(parsed.Host, ":") {
				return "", errors.New("connectors: redirect URL port must be numeric 1-65535")
			}
			parsed.Host = h
		default:
			// Strict digits-only via parsePort (not bare Atoi): "+80"/"080"
			// would otherwise validate yet JoinHostPort re-emits the
			// spelling verbatim — validated != sent, Google exact-mismatches
			// instead of failing fast here. Compared and re-emitted on the
			// numeric value so the stored string is always canonical ("080"
			// sends as "80"; "+"/"-"/space never validate).
			n, ok := parsePort(p)
			if !ok {
				return "", errors.New("connectors: redirect URL port must be numeric 1-65535")
			}
			if (parsed.Scheme == "https" && n == 443) || (parsed.Scheme == "http" && n == 80) {
				// Stripped: default port dropped. Bracket here (no
				// JoinHostPort on this branch) so an IPv6 literal stays a
				// valid host.
				if strings.Contains(h, ":") {
					h = "[" + h + "]"
				}
				parsed.Host = h
			} else {
				// Kept: JoinHostPort brackets IPv6 itself — pass it the bare
				// host, or [::1]:8643 double-brackets to [[::1]]:8643.
				// Re-emitted from the number, so "080" sends as "80".
				parsed.Host = net.JoinHostPort(h, strconv.Itoa(n))
			}
		}
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		// http is loopback-only, and only in these three spellings — the
		// same set .env.example names, no wider. A general IsLoopback
		// check would also accept 127/8 siblings and mapped forms, but
		// every accepted spelling must be registered verbatim in the
		// console, so the allowlist stays exactly the documented set. A
		// non-listed http registration can never complete, and the
		// failure surfaces as redirect_uri_mismatch in the browser, not
		// as a config error — so fail closed here with a pointer at the
		// scheme.
		return "", errors.New("connectors: redirect URL must use https (http loopback-only)")
	}
	if strings.HasSuffix(parsed.Hostname(), ".localhost") {
		// Bare "localhost" only, no subdomains — checked after the scheme
		// gate so http://foo.localhost still reports the scheme (pinned),
		// while https://foo.localhost names the subdomain rule instead of
		// the generic FQDN-shape message below. A subdomain resolves
		// loopback locally but no public registration can hold it, so the
		// fix (use bare localhost) must be obvious.
		return "", errors.New("connectors: redirect URL must use bare localhost, not a subdomain (no public registration can hold *.localhost)")
	}
	if h := parsed.Hostname(); net.ParseIP(h) != nil && !net.ParseIP(h).IsLoopback() {
		// IPs other than loopback can never hold a public Google
		// registration (private/LAN, Tailscale, or public alike): the
		// flow would die with redirect_uri_mismatch in the browser after
		// passing every server-side check. Fail closed here, same class
		// as the userinfo/query/port-strip gates. Google's own redirect-URI
		// validation rules state this directly ("Hosts cannot be raw IP
		// addresses. Localhost IP addresses are exempted"). Loopback stays
		// for direct-serve development (localhost/127.0.0.1/::1, any scheme
		// the scheme gate above already narrowed).
		return "", errors.New("connectors: redirect URL must use a registrable hostname, not a non-loopback IP (use a dotted FQDN; direct-serve with localhost, 127.0.0.1, or [::1])")
	}
	if h := parsed.Hostname(); net.ParseIP(h) == nil && !validRedirectHost(h) {
		// Hostname syntax no console registration can hold: underscores,
		// leading/trailing hyphens, empty labels, non-ASCII (punycode at
		// the console, never Unicode). IP literals bypass label rules
		// (loopback-ness checked above). Hostname() already strips brackets
		// and port, so h arrives bare — no Trim needed. Fail closed here
		// instead of dying with redirect_uri_mismatch in the browser.
		return "", errors.New("connectors: redirect URL host must be a dotted FQDN with a lettered multi-char TLD (no LAN short names: use localhost, an IP, or a dotted name; no all-numeric, single-char, reserved, or Unicode hosts; register the punycode form)")
	}
	if parsed.User != nil {
		// Userinfo would be logged verbatim in the startup line and never
		// match Google's exact-redirect comparison — fail closed rather
		// than storing credentials in the service log.
		return "", errors.New("connectors: redirect URL must not contain userinfo (user:pass@)")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		// Google compares the full redirect_uri exactly: a registered
		// query or fragment would never match what the gateway sends
		// (bare path), so fail here with a pointer at the registration,
		// not in the browser with redirect_uri_mismatch. ForceQuery
		// counts: ".../callback?" carries an empty-but-present query
		// that String() preserves and Google compares.
		return "", errors.New("connectors: redirect URL must have no query or fragment; register the bare callback URL")
	}
	// Inner controls fail closed like every sibling input (state/code/
	// verifier at every layer): url.Parse/String round-trips them, so a
	// control-bearing path would pass validation yet mismatch Google.
	// Raw covers literals; the decoded path covers %00-style encodings
	// that Parse decodes before Clean/String re-encodes. The host needs
	// no unescape step: % is rejected outright above, so no encoded
	// sequence survives to decode. Whitespace is gated explicitly rather
	// than left to the suffix check below: interior spaces fail today via
	// the suffix, but the contract names every rejected class up front.
	if HasControl(trimmed) || !utf8.ValidString(trimmed) || HasControl(parsed.Path) || !utf8.ValidString(parsed.Path) || HasWhitespace(trimmed) || HasWhitespace(parsed.Path) {
		return "", errors.New("connectors: redirect URL must not contain control characters, spaces, or invalid UTF-8")
	}
	// Host is ASCII-gated by validRedirectHost; the path is not — but the
	// console holds the decoded spelling while String() sends the
	// percent-encoded form, so validated != sent by construction on any
	// non-ASCII path. Reject it here like userinfo/query: validated ==
	// sent, or fail closed.
	for _, r := range parsed.Path {
		if r >= 0x80 {
			return "", errors.New("connectors: redirect URL path must be ASCII (register the exact sent spelling, never Unicode)")
		}
	}
	parsed.Path = strings.TrimRight(path.Clean(parsed.Path), "/")
	if !strings.HasSuffix(parsed.Path, "/api/connectors/callback") {
		return "", errors.New("connectors: redirect URL path must end in /api/connectors/callback (the single registered endpoint)")
	}
	// The sent form must match too: EscapedPath re-emits RawPath verbatim,
	// so %2F spellings decode to the suffix above yet send encoded —
	// validated != sent. Suffix-check the escaped form as well.
	if !strings.HasSuffix(parsed.EscapedPath(), "/api/connectors/callback") {
		return "", errors.New("connectors: redirect URL path must not contain encoded slashes; register the decoded spelling")
	}
	// No escapes at all beyond what Clean already normalised: %20, %09,
	// or double-encoding (%2520) decode past the control/space gates on
	// parsed.Path yet String() re-emits different bytes — another
	// validated != sent shape. Registrations are plain ASCII with no
	// encoding needed, so require the escaped form to equal the decoded
	// form outright.
	if parsed.EscapedPath() != parsed.Path {
		return "", errors.New("connectors: redirect URL path must not contain percent-encoding; register the plain spelling")
	}
	// Length-cap the canonical output, not just the raw input: growth
	// through canonicalisation (brackets, unescaping) must not mint a
	// stored/sent string the write paths would 400. Same MaxRedirectLen
	// as every sibling redirect gate, so checked == sent == stored by construction.
	out := parsed.String()
	if len(out) > MaxRedirectLen {
		return "", errors.New("connectors: redirect URL too long")
	}
	return out, nil
}

// validCredential reports whether a trimmed client id/secret is shaped
// like a real credential: no controls, valid UTF-8, and no whitespace at
// all. Edge owner is TrimCredential (forgives edge whitespace + Cf);
// this is the interior owner (rejects what survives trimming). Read them
// together: the next edit must not "fix" one side without the other —
// same split-ownership pattern as the endpoint() trim-contract comment.
func validCredential(s string) bool {
	if HasControl(s) || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, unicode.IsSpace) < 0
}

// missingVars names the unset OAuth env vars for the config-error path,
// so the startup log points at the gap instead of the whole set. The key
// is included: it is validated next (hex shape), so without it an operator
// missing all four would fix three, restart, and hit the key error — one
// trip instead of two.
func missingVars(clientID, clientSecret, redirectURL string, keyPresent bool) []string {
	var missing []string
	if clientID == "" {
		missing = append(missing, "GOOGLE_CLIENT_ID")
	}
	if clientSecret == "" {
		missing = append(missing, "GOOGLE_CLIENT_SECRET")
	}
	if redirectURL == "" {
		missing = append(missing, "GOOGLE_REDIRECT_URL")
	}
	if !keyPresent {
		missing = append(missing, "CONNECTORS_ENCRYPTION_KEY")
	}
	return missing
}

// OAuthConfigFromEnv builds the OAuth client from the environment. ANY
// missing piece is an error (nil config, connect stays 501): a half-open
// flow — auth URL without exchange, exchange without storage key — would
// strand the user mid-consent with no honest error to show.
func OAuthConfigFromEnv() (*OAuthConfig, error) {
	clientID := TrimCredential(os.Getenv("GOOGLE_CLIENT_ID"))
	clientSecret := TrimCredential(os.Getenv("GOOGLE_CLIENT_SECRET"))
	redirectURL := TrimCredential(os.Getenv("GOOGLE_REDIRECT_URL"))
	// Name the missing pieces, not just the set: the gateway logs this
	// verbatim at startup, and an operator who set two of four should not
	// have to restart twice to find both gaps (the key is validated next).
	keyPresent := TrimCredential(os.Getenv(encryptionKeyEnv)) != ""
	if missing := missingVars(clientID, clientSecret, redirectURL, keyPresent); len(missing) > 0 {
		return nil, errors.New("connectors: missing OAuth config: " + strings.Join(missing, ", "))
	}
	// Credentials fail fast on control bytes and invalid UTF-8 like every
	// sibling input (redirect below, state/code/verifier at every layer):
	// an operator-pasted control would otherwise embed in the minted
	// AuthCodeURL and the token request and fail at Google after consent.
	if !validCredential(clientID) || !validCredential(clientSecret) {
		return nil, errors.New("connectors: GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET must not contain control characters, spaces, or invalid UTF-8")
	}
	if len(clientID) > MaxCredentialLen || len(clientSecret) > MaxCredentialLen {
		return nil, errors.New("connectors: GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET too long")
	}
	// Fail closed on a misregistered redirect: Google matches the callback
	// URL exactly, so a typo'd host/port/scheme or a missing
	// /api/connectors/callback suffix dies in the *browser* with
	// redirect_uri_mismatch and nothing in the server log pointing at it.
	// Validate the shape here; the operator compares the logged value
	// against the Cloud console registration. Trailing slashes are trimmed
	// first: splitProfile Cleans the path server-side, so .../callback/
	// would serve fine — rejecting it here would 501 a working config
	// over punctuation.
	parsed, err := CanonicalRedirectURL(redirectURL)
	if err != nil {
		return nil, err
	}
	redirectURL = parsed
	key, err := KeyFromEnv()
	if err != nil {
		return nil, err
	}
	return &OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Key:          key,
		AuthURL:      googleAuthURL,
		TokenURL:     googleTokenURL,
	}, nil
}

func (c *OAuthConfig) endpoint() oauth2.Endpoint {
	// Re-trims what the callers already trimmed: idempotent defense in
	// depth for direct oauth2()/endpoint() callers, not the single trim
	// point — AuthURL/Exchange each trim their own copy first, and the
	// copy-contract comments there describe that side. Do not "dedupe"
	// either layer: removing the helper trim breaks direct callers,
	// removing the copy trim breaks checked == sent.
	return oauth2.Endpoint{AuthURL: TrimCredential(c.AuthURL), TokenURL: TrimCredential(c.TokenURL)}
}

func (c *OAuthConfig) oauth2(scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     TrimCredential(c.ClientID),
		ClientSecret: TrimCredential(c.ClientSecret),
		RedirectURL:  TrimCredential(c.RedirectURL),
		Scopes:       scopes,
		Endpoint:     c.endpoint(),
	}
}

// NewState mints one CSRF state + PKCE verifier pair. State is 128 bits of
// crypto rand, base64url; the verifier is 64 bytes (S256 flow, so the
// exchange needs no client-secret-in-browser — the secret never leaves the
// server, and the app never sees the verifier either). All-or-nothing:
// either rand.Read aborts the pair with ("", "", err) — no partial pair is
// ever minted, so the connect handler 502s without touching the store.
func NewState() (state, verifier string, err error) {
	rb := make([]byte, 16)
	if _, err := rand.Read(rb); err != nil {
		return "", "", err
	}
	vb := make([]byte, 64)
	if _, err := rand.Read(vb); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(vb)
	return base64.RawURLEncoding.EncodeToString(rb), verifier, nil
}

// MaxStateFieldLen bounds state/verifier/code inputs end to end. States are
// 128-bit base64url (~22 chars); PKCE verifiers ~86; Google codes a few
// hundred chars. Anything past 2048 is not a real credential — it is a
// garbage or hostile input that must 400 before it becomes a DB filter
// or an outbound token request. Enforced at the gateway (fast path) AND the
// store (ConsumeState/CreateState) AND Exchange: every layer is exported
// for direct callers (future MCP servers), so the gateway cap alone would
// leave the enforcement to one layer. Plaintext tokens share this bound
// at seal time (EncryptToken); sealed ciphers use MaxCipherLen below,
// since base64 expansion means the two ends of the round-trip cannot
// share one number.
const MaxStateFieldLen = 2048

// MaxCipherLen bounds sealed tokens at rest and on the wire. A separate
// bound from MaxStateFieldLen by necessity: a 2048 B plaintext seals to
// ~2768 chars base64url (12 B nonce + 16 B tag, 4/3 expansion), so one
// shared bound cannot hold both ends of the round-trip — anything
// EncryptToken mints must pass the SetTokens/DecryptToken cap. Sealed
// grants stay far under this in prod; raise only if Google lengthens
// tokens past what fits here.
const MaxCipherLen = 4096

// MaxPlaintextLen bounds the plaintext EncryptToken will seal. Same value
// as MaxStateFieldLen today, deliberately a separate name: the 2048 here
// is about token size (Google refresh tokens are ~100-200 chars; this
// cap only ever fires on garbage or hostile input), while MaxStateFieldLen
// is about OAuth input size (codes, states, verifiers). If Google
// lengthens refresh tokens past 2048, raise this — not the OAuth-input
// bound, which has nothing to do with token size.
const MaxPlaintextLen = 2048

// MaxCredentialLen bounds the OAuth client id/secret at load and at every
// helper copy. Same value as MaxStateFieldLen today, deliberately a
// separate name: the 2048 here is about credential size (client ids and
// secrets are ~30-70 chars; this cap only ever fires on garbage or
// operator paste error), while MaxStateFieldLen is about OAuth input size
// (codes, states, verifiers, redirects). If Google lengthens secrets past
// 2048, raise this — not the OAuth-input bound, which has nothing to do
// with credential size.
const MaxCredentialLen = 2048

// MaxVerifierLen bounds PKCE verifiers: RFC 7636 fixes 43-128 chars and
// NewState mints 86, so anything longer is not a real verifier — it is a
// hand-built or garbage value that would mint a consent URL Google can
// only 400 after the user consented. States and codes keep the looser
// MaxStateFieldLen (no RFC bound); only the verifier gets the RFC cap.
const MaxVerifierLen = 128

// MinVerifierLen is the RFC 7636 floor matching the ceiling above: a
// shorter verifier mints a challenge Google can only 400, stranding the
// consent the same way an oversized one does. Enforced alongside the
// ceiling at every verifier gate.
// MinStateLen floors the same brute-force class for states: NewState mints
// 22-char 128-bit base64url, so anything much shorter is hand-minted —
// and the callback is unauthenticated, so a 1-char state is guessable
// inside the 10-minute window in a way a 22-char state is not. 16 keeps a
// comfortable margin under the minted 22 without pinning the exact
// encoding length.
const MinStateLen = 16
const MinVerifierLen = 43

// MaxRedirectLen bounds the registered redirect URL at parse and at every
// canonical-output check. Same value as MaxStateFieldLen today, deliberately
// a separate name: the 2048 here is about URL size (registrations are ~60
// chars; host+path growth is unbounded in principle), while MaxStateFieldLen
// is about OAuth input size (codes, states, verifiers). If a legitimate
// registration ever needs more room, raise this — not the OAuth-input
// bound, which has nothing to do with URL size.
const MaxRedirectLen = 2048

// AuthURL builds the Google consent URL for one connector attempt. Offline
// access (a refresh token) + consent prompt every time: without
// prompt=consent Google returns a refresh token only on the FIRST grant,
// and a reconnect that yields no refresh token would silently downgrade the
// row to access-token-only with no renewal path.
//
// Fail-closed builder: nil cfg, unknown id, empty/overlong inputs,
// empty endpoints, or a misshapen redirect return "" — cheaper than a
// panic and cheaper than a consent URL that can only die later at
// ConsumeState. cfg normally comes from OAuthConfigFromEnv (which also
// validates the redirect shape and sets both Google endpoints), but a
// hand-built cfg with a non-matching redirect is rejected here too via
// the same CanonicalRedirectURL check — shape is helper-enforced, not just
// caller-promised. logf (may be nil) names which guard fired for
// server-side triage; the app never sees it. The gateway route, CreateState, and Exchange all
// re-check inputs (defense in depth for direct callers), but this
// helper needs no trusted caller.
func AuthURL(cfg *OAuthConfig, id, state, verifier string, logf func(string, ...any)) string {
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without paying the normalize alloc or logging the value.
	if len(id) > MaxConnectorIDLen {
		if logf != nil {
			logf("connectors: auth URL build failed: oversized connector id")
		}
		return ""
	}
	// Folded before every use below, like Exchange and every store verb:
	// the unconfigured log and everything after it observe the same
	// spelling — stored == logged == sent by construction.
	id = NormalizedID(id)
	// Controls fail closed like every sibling input — same guard as
	// Exchange, not just quoted logging: the echo below must never carry
	// them.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		if logf != nil {
			logf("connectors: auth URL build failed: invalid connector id %q", id)
		}
		return ""
	}
	if cfg == nil || TrimCredential(cfg.ClientID) == "" || TrimCredential(cfg.ClientSecret) == "" || TrimCredential(cfg.RedirectURL) == "" {
		if logf != nil {
			logf("connectors: auth URL build failed: oauth client is not configured (%q)", id)
		}
		return ""
	}
	// Work on a trimmed copy from here on: the checks below and the
	// built URL must see identical values. ClientSecret is folded too
	// even though AuthCodeURL never sends it — the copy contract is
	// "everything trimmed", so the next edit copies one pattern.
	trimmed := *cfg
	trimmed.ClientID = TrimCredential(trimmed.ClientID)
	trimmed.ClientSecret = TrimCredential(trimmed.ClientSecret)
	trimmed.RedirectURL = TrimCredential(trimmed.RedirectURL)
	trimmed.AuthURL = TrimCredential(trimmed.AuthURL)
	trimmed.TokenURL = TrimCredential(trimmed.TokenURL)
	// Redirect shape before credentials, matching Exchange: a hand-built
	// cfg with both bad answers redirect-first in both helpers, so the same
	// input pair hits the same guard first whichever helper runs.
	canonical, err := CanonicalRedirectURL(trimmed.RedirectURL)
	if err != nil {
		// Same shape gate as the env path: a hand-built redirect that
		// would never match a registration fails here, not in the
		// browser with redirect_uri_mismatch. Quoted: canonicaliser
		// errors are static today, but the every-value-quoted contract
		// keeps the next edit from landing an input echo unquoted.
		if logf != nil {
			logf("connectors: auth URL build failed for %q: %q", id, err.Error())
		}
		return ""
	}
	// Build from the canonicalised shape, not cfg.RedirectURL verbatim,
	// so AuthURL and Exchange agree on the exact string. Length-gate the
	// canonical form like Exchange/CreateState: canonicalisation can grow
	// the string, and the checked value must be the sent one.
	trimmed.RedirectURL = canonical
	if len(trimmed.RedirectURL) > MaxRedirectLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: oversized redirect", id)
		}
		return ""
	}
	if !validCredential(trimmed.ClientID) || !validCredential(trimmed.ClientSecret) {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: invalid client credential", id)
		}
		return ""
	}
	if len(trimmed.ClientID) > MaxCredentialLen || len(trimmed.ClientSecret) > MaxCredentialLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: oversized client credential", id)
		}
		return ""
	}
	if trimmed.AuthURL == "" || trimmed.TokenURL == "" {
		// A hand-built cfg with no endpoints mints a consent URL Google
		// can only 400. OAuthConfigFromEnv always sets both to the
		// Google endpoints, so this guards direct callers only.
		if logf != nil {
			logf("connectors: auth URL build failed for %q: missing endpoint", id)
		}
		return ""
	}
	if HasControl(trimmed.AuthURL) || HasControl(trimmed.TokenURL) || !utf8.ValidString(trimmed.AuthURL) || !utf8.ValidString(trimmed.TokenURL) || HasWhitespace(trimmed.AuthURL) || HasWhitespace(trimmed.TokenURL) {
		// Same fail-fast class as the credential gate above: a control-
		// bearing endpoint would mint a consent URL Google can only 400,
		// after the user consented.
		if logf != nil {
			logf("connectors: auth URL build failed for %q: invalid endpoint", id)
		}
		return ""
	}
	// Folded and trimmed like every store verb — unknown or empty inputs
	// fail closed, and overlong inputs fail closed too (same
	// MaxStateFieldLen bound the stores enforce): a multi-KB state or
	// verifier would mint a consent URL Google 414s, with only log spam to
	// show for it. The gateway mints fixed-size values via NewState, so
	// this guards direct callers. (id already folded at the top; the
	// state/verifier trims below are the per-call normalisation.)
	// Raw length first: the trims below walk the whole input, so an
	// unbounded hand-built value pays two walks before any gate. Same bound
	// and log as the trimmed gate — trimming only shrinks, so this fires
	// first on the same inputs (plus pathological padding, same "").
	if len(state) > MaxStateFieldLen || len(verifier) > MaxVerifierLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: oversized state or verifier", id)
		}
		return ""
	}
	state = TrimCredential(state)
	verifier = TrimCredential(verifier)
	// Empty → length → Known, matching Exchange/CreateState/ConsumeState/
	// SetTokens: an oversized input for an unknown id 400s on length, so
	// the unknown-id path cannot be told apart by which error fires first.
	if state == "" || verifier == "" {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: empty state or verifier", id)
		}
		return ""
	}
	if len(state) > MaxStateFieldLen || len(verifier) > MaxVerifierLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: oversized state or verifier", id)
		}
		return ""
	}
	if len(state) < MinStateLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: undersized state", id)
		}
		return ""
	}
	if len(verifier) < MinVerifierLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: undersized verifier", id)
		}
		return ""
	}
	if HasControl(state) || HasControl(verifier) || !utf8.ValidString(state) || !utf8.ValidString(verifier) || HasWhitespace(state) || HasWhitespace(verifier) {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: invalid state or verifier", id)
		}
		return ""
	}
	if !Known(id) {
		if logf != nil {
			logf("connectors: auth URL build failed: unknown connector %q", id)
		}
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return trimmed.oauth2(GoogleScopes(id)).AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("access_type", "offline"),
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
}

// Exchange swaps a code (+ PKCE verifier) for tokens against the configured
// token endpoint. cfg may be nil — it fails closed with a 400
// ("not configured"), never a panic. HTTP errors surface as a generic 502
// ("grant exchange failed") with the provider's error_description only in
// the log — oauth2 errors echo the token endpoint verbatim, and the client
// needs a retry signal, not provider internals. logf may be nil (tests).
// The returned Expiry is clamped to now+MaxAccessLifetime (wall clock):
// far-future or skewed provider values renew soon instead of sleeping
// decades — safe for scheduling callers that never persist. The write
// sites re-clamp on their own clocks as the stored owners; callers that
// persist must still go through SetTokens.
func Exchange(ctx context.Context, cfg *OAuthConfig, id, code, verifier string, logf func(string, ...any)) (*oauth2.Token, error) {
	// Nil guard first: everything below touches ctx, and a nil Context
	// would panic on the first use instead of 400ing. A nil Context is a
	// caller bug — fail closed with the discriminant instead of
	// substituting Background (which would silently drop cancellation
	// propagation). Untyped nil only: a typed-nil ctx (non-nil interface
	// holding a nil pointer) still panics on first use like the driver —
	// pass a live (possibly cancelled) ctx, never a nil of either kind.
	if ctx == nil {
		if logf != nil {
			logf("connectors: grant exchange refused: nil context")
		}
		// Deterministic 400 like the dead-ctx branch below (not Upstream):
		// nil never comes from the wire — it is a direct-caller
		// programmer bug in the same "nothing was sent" class, and 502
		// would misread as a flaky upstream worth retrying. The distinct
		// discriminant stays in the log line.
		return nil, NewStoreError("grant exchange aborted, nil context", false)
	}
	// Dead caller before the fallback wrap below (same ordering as the
	// gateway fast paths): a cancelled parent with no deadline must not
	// arm a timer that fires immediately, nor pay the guards below.
	// Deterministic 400, not a 502 — nothing was sent. Intentionally ahead
	// of even config-shape (unlike AuthURL's config-first): timer safety
	// outranks config diagnosis — a dead ctx must never arm the fallback
	// timer, while a config error is still logged on the next live tap.
	if ctx.Err() != nil {
		if logf != nil {
			logf("connectors: grant exchange refused: aborted, ended context")
		}
		return nil, NewStoreError("grant exchange aborted, caller context ended", false)
	}
	// Bound the provider round-trip for direct callers: the gateway always
	// passes its 30s callback ctx, but the helper is exported — an
	// unbounded caller ctx plus a hung token endpoint must not hang a
	// worker forever. A caller deadline stands; only the unbounded case
	// gets the 15s default (under the gateway's 30s, over Google's P99).
	// Deliberately shorter than the gateway budget: direct callers fail
	// fast on a hung endpoint, the gateway owns the patient path — the
	// callback ctx is the one place a slow Google P99 is worth waiting for.
	// Upper-bounded too: a caller deadline beyond the gateway budget is
	// capped to it, so no direct/future caller can hang a worker longer
	// than the callback path itself tolerates.
	// boundName feeds the timeout log below: same 502 either way, but
	// triage greps one wording for the fault and the name for the layer.
	boundName := "caller"
	if dl, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		boundName = "15s-internal"
	} else if time.Until(dl) > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		boundName = "30s-cap"
	}
	// Length before fold and before Known, matching AuthURL and every
	// store verb: an oversized id is certainly unknown. Logged without
	// the value: quoting megabytes would reintroduce the log alloc this
	// gate exists to avoid.
	if len(id) > MaxConnectorIDLen {
		if logf != nil {
			logf("connectors: grant exchange refused: oversized connector id")
		}
		return nil, NewStoreError("oversized connector id", false)
	}
	// Folded first so every deny below observes the same spelling —
	// stored == logged by construction (AuthURL folds first for the same
	// reason). Folding is not an identity check, so shape-before-identity
	// ordering is unchanged.
	id = NormalizedID(id)
	// Every deterministic reject below logs its discriminant, mirroring
	// AuthURL's per-guard logf: the client gets the mapped 400/404, and
	// triage gets the guard name without guessing.
	deny := func(reason string) *StoreError {
		if logf != nil {
			logf("connectors: grant exchange refused for %q: %s", id, reason)
		}
		return NewStoreError(reason, false)
	}
	// Controls fail closed like every sibling input, before any shape
	// check: a control-bearing id would otherwise ride the NotFound echo
	// below into the response body. Checked post-fold so edge Cf forgiven
	// by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		return nil, deny("invalid connector id")
	}
	// Config shape after the dead-ctx guard above (both deterministic,
	// no I/O): a live caller with a bad config still logs the
	// misconfiguration — only a dead caller short-circuits first, so no
	// timer ever arms on an ended parent.
	if cfg == nil {
		return nil, deny("oauth client is not configured")
	}
	// Bounded before the token endpoint, on trimmed values: codes/verifiers
	// are short by construction, and a multi-MB value would become an
	// oversized outbound request. Trimmed first so padding can't push a
	// real credential over the cap (or slip whitespace through to Google).
	// Deterministic input reject (400), not a flaky upstream — same shape
	// as the missing-refresh branch below.
	// Raw length first like AuthURL: the trims below walk the whole input.
	// Same bound and deny as the trimmed gate — trimming only shrinks.
	if len(code) > MaxStateFieldLen || len(verifier) > MaxVerifierLen {
		return nil, deny("oauth code or verifier too long")
	}
	code = TrimCredential(code)
	verifier = TrimCredential(verifier)
	if code == "" || verifier == "" {
		// Deterministic caller error, not an upstream failure: nothing was
		// sent anywhere. Same 400 shape as the length and no-refresh
		// branches.
		return nil, deny("oauth code or verifier is empty")
	}
	if len(code) > MaxStateFieldLen || len(verifier) > MaxVerifierLen {
		return nil, deny("oauth code or verifier too long")
	}
	// No floor on code (unlike state/verifier): Google defines no minimum,
	// so a short code reaches the token endpoint and fails there with its
	// own discriminant instead of burning as a shape reject.
	if len(verifier) < MinVerifierLen {
		return nil, deny("oauth verifier too short")
	}
	if HasControl(code) || HasControl(verifier) || !utf8.ValidString(code) || !utf8.ValidString(verifier) || HasWhitespace(code) || HasWhitespace(verifier) {
		return nil, deny("oauth code or verifier invalid")
	}
	// Config-shape before identity, matching AuthURL: a bad redirect or
	// blank client fields 400s on shape the same way whether the id is
	// known or not, so the unknown-id path cannot be told apart by which
	// guard fires first. Canonicalise the redirect the same way AuthURL
	// does: the token endpoint compares redirect_uri exactly, so both
	// helpers must mint and send the same string. Trimmed like every other
	// input: padded client fields are a caller bug, and the checked
	// values must match what is sent.
	redirect, err := CanonicalRedirectURL(cfg.RedirectURL)
	if err != nil {
		// Static to the client, detailed in the log: every other
		// provider/config-shape failure on this path returns a fixed
		// discriminant (so a future canonicaliser message that
		// interpolates input can never reach the callback caller), with
		// the check name log-only for triage. AuthURL does the same
		// (logs the check, returns "").
		if logf != nil {
			logf("connectors: grant exchange refused for %q: invalid redirect (%s)", id, err.Error())
		}
		return nil, NewStoreError("oauth client redirect URL is invalid", false)
	}
	// Checked == sent: canonicalisation can grow the string (brackets),
	// so length-gate the canonical form too — same as both CreateStates.
	if len(redirect) > MaxRedirectLen {
		return nil, deny("oauth client redirect URL too long")
	}
	cc := *cfg
	cc.ClientID = TrimCredential(cc.ClientID)
	cc.ClientSecret = TrimCredential(cc.ClientSecret)
	cc.AuthURL = TrimCredential(cc.AuthURL)
	cc.TokenURL = TrimCredential(cc.TokenURL)
	if !validCredential(cc.ClientID) || !validCredential(cc.ClientSecret) {
		return nil, deny("oauth client credential is invalid")
	}
	if len(cc.ClientID) > MaxCredentialLen || len(cc.ClientSecret) > MaxCredentialLen {
		return nil, deny("oauth client credential too long")
	}
	// Length gate above covers the trimmed values copied here — keep the
	// copy below the gate, or padding could push a real credential over
	// the cap (the padded-push class fixed for the trim-copy contract).
	if cc.ClientID == "" || cc.ClientSecret == "" {
		return nil, deny("oauth client is not configured")
	}
	if cc.AuthURL == "" || cc.TokenURL == "" {
		return nil, deny("oauth client endpoints are not configured")
	}
	if HasControl(cc.AuthURL) || HasControl(cc.TokenURL) || !utf8.ValidString(cc.AuthURL) || !utf8.ValidString(cc.TokenURL) || HasWhitespace(cc.AuthURL) || HasWhitespace(cc.TokenURL) {
		return nil, deny("oauth client endpoint is invalid")
	}
	if !Known(id) {
		if logf != nil {
			logf("connectors: grant exchange refused: unknown connector %q", id)
		}
		// Static message: every other deny() in this helper is a fixed
		// string, and the gateway already logs the id with %q — the domain
		// message carries no caller-influenced text on any branch.
		return nil, &StoreError{Msg: "unknown connector", NotFound: true}
	}
	cc.RedirectURL = redirect
	tok, err := cc.oauth2(GoogleScopes(id)).Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			// Name the timeout at the source: direct callers never see
			// the gateway's per-verb timeout wording, so the generic 502
			// below would read as a store failure. Same string the gateway
			// answers with ("did not answer in time"), so one grep finds
			// both halves of the same slow-endpoint fault — and the bound
			// name says which timer fired (internal vs caller). The gateway's
			// own ctx check fires first on its path, so this branch is for
			// direct/future callers only.
			if logf != nil {
				logf("connectors: grant exchange timed out for %q (%s bound)", id, boundName)
			}
			return nil, NewStoreError("the grant exchange did not answer in time", true)
		}
		if ctx.Err() == context.Canceled {
			// Mid-flight cancel is a deterministic abort, not a flaky
			// upstream: nothing was received, so 400 like the pre-flight
			// dead-ctx branch — never the generic 502 below.
			return nil, deny("grant exchange aborted, caller context ended")
		}
		if logf != nil {
			// Scrubbed + capped like the domain mapper below — and
			// quoted, so an inner control byte in provider text cannot
			// split the log line.
			logf("connectors: grant exchange failed for %q: %q", id, approvals.ScrubError(err.Error()))
		}
		return nil, NewStoreError("grant exchange failed", true)
	}
	if tok == nil {
		// oauth2 contractually returns non-nil on success, but a nil token
		// with nil error (test stub, future transport wrapper) must not
		// panic the worker — fail closed like every other input leg.
		return nil, deny("grant exchange returned no token")
	}
	if TrimCredential(tok.RefreshToken) == "" {
		// No refresh token means no renewal path: storing this row as
		// connected would strand it at the first access-token expiry.
		// Fail the connect loudly instead of a slow-burn disconnect.
		// Deterministic provider answer (not a flaky upstream), so 400 —
		// retrying the same consumed code cannot change the outcome, and
		// triage must read this as misconfigured consent, not an outage.
		// Trimmed like SetTokens: same definition of "missing" on both
		// sides of the store boundary.
		if logf != nil {
			logf("connectors: grant exchange refused: no refresh token in provider response")
		}
		return nil, NewStoreError("grant exchange returned no refresh token; reconnect and approve offline access", false)
	}
	// Normalise before return: the checked strings and the delivered
	// bytes must be identical, so a future caller that seals
	// tok.RefreshToken raw cannot seal padding the refresher can't use
	// (same class as the gateway's trim-before-seal, enforced here so
	// no caller has to remember it).
	tok.RefreshToken = TrimCredential(tok.RefreshToken)
	tok.AccessToken = TrimCredential(tok.AccessToken)
	// Controls fail closed like every sibling input: a control-bearing
	// provider token would 502 at seal time (state already consumed)
	// instead of 400ing here as the deterministic answer it is.
	if HasControl(tok.RefreshToken) || HasControl(tok.AccessToken) || !utf8.ValidString(tok.RefreshToken) || !utf8.ValidString(tok.AccessToken) || HasWhitespace(tok.RefreshToken) || HasWhitespace(tok.AccessToken) {
		return nil, deny("grant exchange returned an invalid token")
	}
	// Length-capped like the ciphertext it will become: a multi-MB token
	// would otherwise 502 at seal time after the state is consumed.
	// Same MaxPlaintextLen the seal gate enforces, checked here first.
	if len(tok.RefreshToken) > MaxPlaintextLen || len(tok.AccessToken) > MaxPlaintextLen {
		return nil, deny("grant exchange returned an oversized token")
	}
	// Single-representation like the gateway and both SetTokens: an
	// expiry-without-access pair is rejected downstream, so zero the clock
	// when the cipher is empty — before the clamp below — instead of
	// delivering a pair no store accepts. Checked == delivered end to end:
	// readers key off the cipher (TokenRow), never a bare timestamp.
	if tok.AccessToken == "" {
		tok.Expiry = time.Time{}
	}
	// Clamp the returned Expiry too: the write sites re-clamp on their own
	// clocks (single owners), but a scheduling caller that uses tok.Expiry
	// without persisting would otherwise sleep decades on a far-future or
	// skewed provider value. Same unbounded-lifetime class as the state
	// clamp — fail safe (renew soon), never far. Wall-clock by design and
	// advisory only: the stores re-clamp authoritatively on their own
	// clocks, so a stepped fake clock and this gate never have to agree
	// (see ClampAccessExpiry's single-owner contract — this call site is a
	// reader, not an owner).
	// Zero stays zero: a clamped-to-zero (negative floored) expiry is the
	// renew-now sentinel (time.Time{}), not the epoch — stored == delivered.
	if !tok.Expiry.IsZero() {
		if clamped := ClampAccessExpiry(tok.Expiry.UnixMilli(), time.Now()); clamped <= 0 {
			tok.Expiry = time.Time{}
		} else {
			tok.Expiry = time.UnixMilli(clamped)
		}
	}
	// Persist via SetTokens (the write sites own the stored clamp); this
	// return is now clamped for scheduling callers too.
	return tok, nil
}

// TokenRow is the ciphertext half of a connector grant. The store holds
// these strings opaquely — it never sees plaintext and needs no key.
// Encryption lives with the OAuth layer (EncryptToken/DecryptToken) and the
// future MCP servers, which share the key from the environment.
type TokenRow struct {
	// RefreshCipher is base64(nonce||ciphertext) of the OAuth refresh token.
	// Never empty on a stored row: Exchange refuses token responses without
	// one, so a stored grant always renews.
	RefreshCipher string `bson:"refreshToken" json:"-"`
	// AccessCipher is base64(nonce||ciphertext) of the short-lived access token.
	// May be empty ("" literal, never a sealed empty — EncryptToken refuses
	// those, as it refuses padded plaintext: stored ciphers are always
	// trimmed base64url, so checked == stored end to end): the refresher
	// treats empty as "renew now". The refresh cipher above must never be
	// empty — same rule the store enforces — or the row strands at the
	// first access-token expiry.
	AccessCipher string `bson:"accessToken" json:"-"`
	// AccessExpiryUnixMilli is when the access token stops working; the
	// refresher renews before this, not after it fails. Zero means unknown
	// (provider omitted expires_in) — treat as already expired (renew
	// immediately), never as "never expires". Unknown is fail-closed.
	// A past timestamp means the same as zero (renew immediately) by
	// accident of comparison, not by a separate contract: clock skew or
	// a stubby test token lands here, and the refresher cannot tell it
	// from a genuinely stale row — nor does it need to. Far-future values
	// are clamped to now+MaxAccessLifetime at write time — enforced in
	// three places (gateway connect + both SetTokens) plus the Exchange
	// return clamp for scheduling callers, so a stored expiry
	// never reads as "don't renew for decades". The clamp runs on the
	// writer's clock — wall everywhere except memory-test timeNow — so
	// the three write sites never have to agree with each other, only to
	// run the same helper (ClampAccessExpiry). A hand-edited far-future
	// expiry is still served as-is until the next write (GetTokens does
	// not re-clamp): the refresher must clamp on read
	// (min(expiry, now+MaxAccessLifetime)) instead of trusting the stored
	// value — same single-owner rationale as DecryptToken-is-read-enforcement.
	// Readers must treat
	// AccessCipher == "" as missing regardless of Expiry: the write path
	// guarantees the pair (expiry-without-access is rejected), the read
	// path does not re-enforce it — a pre-cap/hand-edited timestamp on a
	// missing token is served as-is, and the refresher keys off the cipher,
	// not the clock.
	AccessExpiryUnixMilli int64 `bson:"accessExpiry" json:"-"`
}

// OAuthState is one pending connect attempt: connector + PKCE verifier,
// consumed (deleted) on first callback so a code can never be exchanged twice.
type OAuthState struct {
	State     string    `bson:"_id" json:"-"`
	Connector string    `bson:"connector" json:"-"`
	Verifier  string    `bson:"verifier" json:"-"`
	ExpiresAt time.Time `bson:"expiresAt" json:"-"`
	// RedirectURI pins the exact redirect sent at mint time, so a config
	// rotation mid-consent cannot strand the attempt with a mismatch.
	// Empty means minted before this column existed: fall back to the
	// live config instead of failing an otherwise valid attempt.
	RedirectURI string `bson:"redirectUri,omitempty" json:"-"`
}
