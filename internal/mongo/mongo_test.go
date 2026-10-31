package mongo

import "testing"

// The pool defaults are the whole point of #424 Stage 1, and they are easy to
// lose: they are applied before ApplyURI, so reordering the option chain
// silently reverts every process to the driver's 100-per-member default with no
// visible change anywhere. These tests pin the ordering.
//
// Both dial paths (Client and ConnectURI) go through clientOptions, which is
// what makes capping here enough for all ~20 processes — most domain servers
// dial through mongostore.Open -> ConnectURI, not Client.
func TestClientOptionsAppliesPoolDefaults(t *testing.T) {
	opts := clientOptions("mongodb://example.invalid:27017/")
	if opts.MaxPoolSize == nil {
		t.Fatal("MaxPoolSize unset — the driver default of 100 per member is back")
	}
	if got := *opts.MaxPoolSize; got != DefaultMaxPoolSize {
		t.Errorf("MaxPoolSize = %d, want %d", got, DefaultMaxPoolSize)
	}
	if opts.MinPoolSize == nil {
		t.Fatal("MinPoolSize unset — it falls through to the driver default")
	}
	if got := *opts.MinPoolSize; got != DefaultMinPoolSize {
		t.Errorf("MinPoolSize = %d, want %d", got, DefaultMinPoolSize)
	}
}

// An explicit pool size in the connection string is operator intent and must
// win: a deployment that legitimately needs a bigger pool has no other lever,
// and silently overriding it would be a trap.
func TestClientOptionsExplicitURIPoolWins(t *testing.T) {
	opts := clientOptions("mongodb://example.invalid:27017/?maxPoolSize=11&minPoolSize=3")
	if opts.MaxPoolSize == nil || *opts.MaxPoolSize != 11 {
		t.Errorf("MaxPoolSize = %v, want 11 from the URI", opts.MaxPoolSize)
	}
	if opts.MinPoolSize == nil || *opts.MinPoolSize != 3 {
		t.Errorf("MinPoolSize = %v, want 3 from the URI", opts.MinPoolSize)
	}
}

// The pre-existing options must survive the refactor: server selection was
// tightened to 8s long before this change and a silent revert would turn a
// fast, named failure into a slow one.
func TestClientOptionsKeepsServerSelectionTimeout(t *testing.T) {
	opts := clientOptions("mongodb://example.invalid:27017/")
	if opts.ServerSelectionTimeout == nil ||
		opts.ServerSelectionTimeout.Seconds() != 8 {
		t.Errorf("ServerSelectionTimeout = %v, want 8s", opts.ServerSelectionTimeout)
	}
	if opts.RetryWrites == nil || !*opts.RetryWrites {
		t.Errorf("RetryWrites = %v, want true", opts.RetryWrites)
	}
}
