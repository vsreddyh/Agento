// Package connectors is the first-connectors foundation behind #316.
//
// v1 covers two connectors only: Gmail (search/read + draft) and Calendar
// (list/upcoming + draft event). Each connects/disconnects independently
// from the app, with explicit read vs draft/write scopes.
// Disconnect stops future access; already-fetched copies already in chat
// history are not retroactively deleted (cf. Dots delete-the-Dot semantics: stopping the tap, not wiping
// history).
//
// Token placement (acceptance: docs state it): OAuth refresh tokens live
// server-side in Mongo (the `connectors` collection, sibling to approvals,
// runs and memory), never in the app and never pasted in chat. The app sees
// status + scopes + reconnect prompts only. Without Mongo both sides fall
// back to memory that cannot meet — same contract as approvals: the list
// still answers, but nothing survives a restart.
//
// Draft-only (v1): Gmail/Calendar send paths stay refused until the
// approval-gate flow covers them. An attempted send is refused with a clear
// message naming drafts as the supported path, not silently gated.
package connectors

import (
	"strings"
	"unicode"
)

// Collection holds one document per connector id. Fixed ids, so the table
// has at most two rows — no TTL, no per-profile split (connectors are
// deployment-wide like approvals, unlike memory/teach).
const Collection = "connectors"

// Known connector ids (v1 scope: these two, nothing else).
const (
	Gmail    = "gmail"
	Calendar = "calendar"
)

// listIDs is the fixed v1 set, in display order. Unexported so no importer
// can mutate the API shape: ListIDs() returns a fresh copy on every call.
var listIDs = [2]string{Gmail, Calendar}

// ListIDs returns the fixed v1 connector ids in display order. A copy, so
// callers cannot mutate the package set.
func ListIDs() []string {
	return append([]string(nil), listIDs[:]...)
}

// DisplayName names a connector for the app. Unknown ids answer "Unknown":
// the gateway 404s them first, so this is a display fallback, never an
// echo of caller text. Folds like every store verb and OAuth helper: same
// logical id, same answer, wherever the caller normalised.
func DisplayName(id string) string {
	if len(id) > maxConnectorIDLen {
		return "Unknown"
	}
	id = NormalizedID(id)
	switch id {
	case Gmail:
		return "Gmail"
	case Calendar:
		return "Calendar"
	default:
		return "Unknown"
	}
}

// ScopesFor names the explicit scopes a connector requests — app-display
// vocabulary, not Google scope strings. Read vs draft/write is the point:
// the app shows these at connect time, and disconnect revokes them
// together. Folds like every store verb: same logical id, same answer.
func ScopesFor(id string) []string {
	if len(id) > maxConnectorIDLen {
		return nil
	}
	switch NormalizedID(id) {
	case Gmail:
		return []string{"gmail.read", "gmail.draft"}
	case Calendar:
		return []string{"calendar.read", "calendar.draft"}
	default:
		return nil
	}
}

// maxConnectorIDLen bounds connector ids below the gateway: ids are two
// short fixed strings, so anything longer cannot match a known id. Same
// value as the gateway's segment cap (which cannot be shared: the gateway
// imports this package, not vice versa). Checked before Normalise/fold so
// a multi-MB id fails without a lowercase+trim alloc or a quoted log line
// for the 404 it was always going to get.
const maxConnectorIDLen = 64

// MaxSegmentAllocLen bounds raw gateway path segments before any fold or
// scan. Deliberately separate from both maxConnectorIDLen (64, the
// identity contract) and MaxStateFieldLen (the OAuth-input bound): it
// guards allocs only, and must merely exceed the contract gate — so
// raising the OAuth-input bound for a future long code must never
// silently move this path gate. Same value as MaxStateFieldLen today,
// independent name on purpose (see MaxCipherLen/MaxPlaintextLen for the
// same split pattern).
const MaxSegmentAllocLen = 2048

// maxToolNameLen bounds tool names below the fold: ids and tool names are
// different namespaces with different length realities (tool names are
// operator-defined and longer), so the tool cap is deliberately roomier
// than the id cap. Either way oversized refuses outright — fail-closed,
// like every sibling length gate. Upstream-owned names are the known
// exception to "rename, don't widen": a wired MCP server whose tool names
// come from upstream cannot be renamed here, so widening the cap (with a
// test update) is the fix for a name this repo doesn't own.
const maxToolNameLen = 128

// Known reports whether id is a v1 connector. Folds like every sibling
// helper: same logical id, same answer — the fold lives here instead of
// relying on every caller to pre-fold before branching on it.
func Known(id string) bool {
	if len(id) > maxConnectorIDLen {
		return false
	}
	switch NormalizedID(id) {
	case Gmail, Calendar:
		return true
	}
	return false
}

// HasWhitespace reports any Unicode whitespace in s. Server-minted
// base64url values (states, verifiers) and Google-issued codes never
// contain whitespace, so an interior space is operator-paste or garbage:
// fail closed as a deterministic 400, not a 502 at Google after consent.
// Checked on trimmed values (edges already forgiven by TrimCredential),
// so any hit here is interior by construction. Tab is already control-
// caught by HasControl; this covers the rest (plain space, no-break
// space, and friends).
func HasWhitespace(s string) bool {
	return strings.IndexFunc(s, unicode.IsSpace) >= 0
}

// HasControl reports inner control bytes (NUL/CR/LF and friends) plus
// invisible format characters (zero-width spaces, BOM, bidi overrides):
// none are ever valid in an id, state, verifier, code, URL, or token.
// TrimSpace strips edges, not interiors — and RuneError-decoding means
// invalid UTF-8 is caught by the separate utf8.ValidString gates, not
// here — so an embedded byte fails closed instead of becoming a DB
// filter or an outbound request. Exported so the gateway reuses this
// single definition instead of carrying its own copy.
func HasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}

// Status is a connector's lifecycle state.
type Status string

const (
	// StatusConnected can serve reads/drafts.
	StatusConnected Status = "connected"
	// StatusDisconnected is the rest state: no future access.
	StatusDisconnected Status = "disconnected"
	// StatusExpired needs a reconnect: the app prompts, never asks for a
	// secret in chat.
	StatusExpired Status = "expired"
)

// Valid reports whether s is a status this package knows.
func (s Status) Valid() bool {
	switch s {
	case StatusConnected, StatusDisconnected, StatusExpired:
		return true
	}
	return false
}

// Connector is one row: status + scopes, never secrets. Refresh tokens live
// in the same Mongo document (see store_mongo.go) but are never returned by
// any read — the wire shape below is what the app is allowed to see.
type Connector struct {
	ID          string   `bson:"_id" json:"id"`
	DisplayName string   `bson:"displayName" json:"display_name"`
	Scopes      []string `bson:"scopes" json:"scopes"`
	Status      Status   `bson:"status" json:"status"`
	LastError   string   `bson:"lastError,omitempty" json:"last_error,omitempty"`

	ConnectedAt int64 `bson:"connectedAt,omitempty" json:"connected_at,omitempty"`
	UpdatedAt   int64 `bson:"updatedAt,omitempty" json:"updated_at,omitempty"`
}

// NormalizedID lowercases and trims a path id, so " GMail " addresses the
// gmail connector rather than 404ing on case or spacing. Unknown ids still
// 404 after this (see Known): normalisation only folds spellings of a real
// connector, it never invents one.
func NormalizedID(raw string) string {
	return strings.ToLower(TrimCredential(raw))
}

// TrimCredential trims edge whitespace AND invisible format characters
// (zero-width spaces, BOM): an operator-pasted id/secret with a trailing
// zero-width char fails HasControl otherwise, reading as a control
// violation for what TrimSpace was supposed to forgive. Interior controls
// still fail closed — trimming is edges-only, like TrimSpace.
func TrimCredential(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	})
}

// IsDraftOnlySend reports whether (server, tool) is a send path that v1
// refuses outright. Draft tools run (free or gated by the relay); send tools
// never reach the server — the relay answers Pi with RefusalMessage.
//
// Case-insensitive on both parts: r.server comes from the operator-controlled
// --server flag, and an exact-case match would let a "Gmail" mcp.json entry
// bypass both this refusal and the approval backstop. Same hardening the
// follow-up mcp.json wiring must keep (pin lowercase names).
//
// Follow-up obligation: keep this list in sync with the wired servers'
// real tool names. Pinned by TestIsDraftOnlySendMatchesServers alongside
// the scope test — a renamed tool fails loudly instead of silently
// missing the refusal and falling through to the gated backstop
// (fail-closed: holds for approval instead of refusing with the draft
// message). Safe, but a UX divergence worth a sync check when the
// connector MCP servers land.
func IsDraftOnlySend(server, tool string) bool {
	// NormalizedID, not a local fold: same composition as every sibling,
	// so the next fold change cannot strand this one. Both parts gated
	// (separate caps: ids are two fixed strings, tool names are
	// operator-defined). Oversize defers to identity: an unknown server
	// cannot own a send path, so it falls through to the gated backstop;
	// a known server with a garbage tool refuses outright, since no legit
	// tool of that server is that long. Precisely: known servers refuse,
	// unknown servers gate — a genuinely-long future read tool is refused
	// as a send only on a known server (rename, don't widen).
	if len(server) > maxConnectorIDLen || len(tool) > maxToolNameLen {
		return Known(server)
	}
	server = NormalizedID(server)
	tool = NormalizedID(tool)
	switch server {
	case "gmail":
		switch tool {
		case "send_message", "send_gmail":
			return true
		}
	case "calendar":
		switch tool {
		case "create_event", "send_invite":
			return true
		}
	}
	return false
}

// RefusalMessage is what Pi sees when a draft-only connector is asked to
// send. It names the supported path (drafts) so the turn continues usefully
// instead of dying on a bare denial.
const RefusalMessage = "sending is disabled for this connector in v1; create a draft instead and let the user send it"

// StoreError is the domain error the HTTP layer maps onto status codes.
// upstream=true renders 502 (BadGateway) via connectorsError; false renders
// 4xx (NotFound when NotFound, else BadRequest). Get the bool right at the
// call site: it is the status code.
//
// Trust boundary: the GATEWAY is the control-byte boundary (HasControl
// rejects before the store). Msg may carry caller-influenced text (e.g.
// "unknown connector a\nb" from a direct store caller); gateway echoes are
// scrubbed+bounded, but store callers must not log Msg raw.
type StoreError struct {
	Msg      string
	NotFound bool
	Upstream bool
}

func (e *StoreError) Error() string { return e.Msg }

// NewStoreError builds a caller-facing store error.
func NewStoreError(msg string, upstream bool) *StoreError {
	return &StoreError{Msg: msg, Upstream: upstream}
}
