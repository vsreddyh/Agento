package mongo

import "testing"

// Every default in clientOptions is applied before ApplyURI, which is what makes
// "an explicit URI wins" one rule instead of two classes of default. These tests
// pin the ordering for each knob, because reordering silently reverts the whole
// stack to the driver's defaults with no visible change anywhere.
//
// Both dial paths (Client and ConnectURI) go through clientOptions, which is what
// makes capping here enough for all ~18 processes after the Stage 1 unwire
// (~20 before): most domain servers dial through mongostore.Open -> ConnectURI,
// not Client.
func TestClientOptionsAppliesPoolDefaults(t *testing.T) {
	opts := clientOptions("mongodb://example.invalid:27017/")
	if opts.MaxPoolSize == nil {
		t.Fatal("MaxPoolSize unset — the driver default of 100 per member is back")
	}
	if got := *opts.MaxPoolSize; got != defaultMaxPoolSize {
		t.Errorf("MaxPoolSize = %d, want %d", got, defaultMaxPoolSize)
	}
	if opts.MinPoolSize == nil {
		t.Fatal("MinPoolSize unset — it falls through to the driver default")
	}
	if got := *opts.MinPoolSize; got != defaultMinPoolSize {
		t.Errorf("MinPoolSize = %d, want %d", got, defaultMinPoolSize)
	}
}

// An explicit value in the connection string is operator intent and must win —
// for every knob, not just the pool. A deployment that legitimately needs a
// bigger pool or a longer selection timeout has no other lever, and silently
// overriding it would be a trap in exactly the incident where someone reaches
// for it.
func TestClientOptionsExplicitURIWins(t *testing.T) {
	opts := clientOptions("mongodb://example.invalid:27017/" +
		"?maxPoolSize=11&minPoolSize=3&serverSelectionTimeoutMS=3000&retryWrites=false")

	if opts.MaxPoolSize == nil || *opts.MaxPoolSize != 11 {
		t.Errorf("MaxPoolSize = %v, want 11 from the URI", opts.MaxPoolSize)
	}
	if opts.MinPoolSize == nil || *opts.MinPoolSize != 3 {
		t.Errorf("MinPoolSize = %v, want 3 from the URI", opts.MinPoolSize)
	}
	if opts.ServerSelectionTimeout == nil || opts.ServerSelectionTimeout.Milliseconds() != 3000 {
		t.Errorf("ServerSelectionTimeout = %v, want 3s from the URI", opts.ServerSelectionTimeout)
	}
	if opts.RetryWrites == nil || *opts.RetryWrites {
		t.Errorf("RetryWrites = %v, want false from the URI", opts.RetryWrites)
	}
}

// The pre-existing defaults must survive the refactor: server selection was
// tightened to 8s long before #424, and a silent revert would turn a fast,
// named failure into a slow one.
func TestClientOptionsKeepsServerSelectionAndRetryDefaults(t *testing.T) {
	opts := clientOptions("mongodb://example.invalid:27017/")
	if opts.ServerSelectionTimeout == nil || opts.ServerSelectionTimeout.Seconds() != 8 {
		t.Errorf("ServerSelectionTimeout = %v, want 8s", opts.ServerSelectionTimeout)
	}
	if opts.RetryWrites == nil || !*opts.RetryWrites {
		t.Errorf("RetryWrites = %v, want true", opts.RetryWrites)
	}
}
