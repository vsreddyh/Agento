// Package connectors is the first-connectors foundation behind #316.
//
// v1 covers three connectors only: Gmail (search/read + draft), Calendar
// (list/upcoming + draft event) and Drive (read/list + save agent-produced
// file). Each connects/disconnects independently from the app, with explicit
// read vs draft/write scopes. Disconnect stops future access; already-fetched
// copies already in chat history or saved files are not retroactively
// deleted (cf. Dots delete-the-Dot semantics: stopping the tap, not wiping
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
)

// Collection holds one document per connector id. Fixed ids, so the table
// has at most three rows — no TTL, no per-profile split (connectors are
// deployment-wide like approvals, unlike memory/teach).
const Collection = "connectors"

// Known connector ids (v1 scope: these three, nothing else).
const (
	Gmail    = "gmail"
	Calendar = "calendar"
	Drive    = "drive"
)

// listIDs is the fixed v1 set, in display order. Unexported so no importer
// can mutate the API shape: ListIDs() returns a fresh copy on every call.
var listIDs = [3]string{Gmail, Calendar, Drive}

// ListIDs returns the fixed v1 connector ids in display order. A copy, so
// callers cannot mutate the package set.
func ListIDs() []string {
	return append([]string(nil), listIDs[:]...)
}

// DisplayName names a connector for the app. Unknown ids echo back: the
// gateway 404s them first, so this is a display fallback, not a lookup.
func DisplayName(id string) string {
	switch id {
	case Gmail:
		return "Gmail"
	case Calendar:
		return "Calendar"
	case Drive:
		return "Drive"
	default:
		return id
	}
}

// ScopesFor names the explicit scopes a connector requests. Read vs
// draft/write is the point: the app shows these at connect time, and
// disconnect revokes them together.
func ScopesFor(id string) []string {
	switch id {
	case Gmail:
		return []string{"gmail.read", "gmail.draft"}
	case Calendar:
		return []string{"calendar.read", "calendar.draft"}
	case Drive:
		return []string{"drive.read", "drive.write"}
	default:
		return nil
	}
}

// Known reports whether id is a v1 connector.
func Known(id string) bool {
	switch id {
	case Gmail, Calendar, Drive:
		return true
	}
	return false
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
	return strings.ToLower(strings.TrimSpace(raw))
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
// Follow-up obligation: keep this list in sync with the wired servers' real
// tool names. The approvals completeness test pins gatedTools, but nothing
// pins THIS list — a renamed tool (sendMessage, send_email) misses the
// refusal and falls through to the gated backstop (fail-closed: holds for
// approval instead of refusing with the draft message). Safe, but a UX
// divergence worth a sync check when the connector MCP servers land.
func IsDraftOnlySend(server, tool string) bool {
	server = strings.ToLower(strings.TrimSpace(server))
	tool = strings.ToLower(strings.TrimSpace(tool))
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
