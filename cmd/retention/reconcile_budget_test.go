package main

import (
	"context"
	"testing"
	"time"
)

// The reconciler's context budget (#180).
//
// The bug these pin: the pass was given `context.WithTimeout(parent, 45s)`, which
// reads like an own budget and is not one. The parent is retention's 60s, already
// partly spent on the money and health deletes, so on a slow night the reconciler
// inherited the remainder — possibly nothing — and died with "context deadline
// exceeded".
//
// The consequence is the specific shape that matters. The reconciler is read-only by
// design: it never repairs, only reports. So a starved context does not produce a
// partial answer, it produces NO answer, and a night where a repeat quietly stopped
// recurring looks exactly like a healthy night.

// A parent with no time left must still yield a live context. This is the whole
// point: the report has to run even when the job around it is out of budget.
func TestReconcileBudgetSurvivesAnExhaustedParent(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelParent()
	time.Sleep(10 * time.Millisecond) // let the parent's deadline actually pass

	if parent.Err() == nil {
		t.Fatal("test setup: the parent should be expired by now")
	}
	ctx, cancel := reconcileBudget(parent)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("the reconciler inherited the caller's exhausted budget: %v", err)
	}
	// And it really is usable, not merely non-nil.
	select {
	case <-ctx.Done():
		t.Fatalf("the reconciler context is already done: %v", ctx.Err())
	default:
	}
}

// A parent that is CANCELED must not take the report down with it, for the same
// reason: a report that stops because something unrelated was cancelled is silent.
func TestReconcileBudgetSurvivesACancelledParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx, cancel := reconcileBudget(parent)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("a cancelled parent killed the reconciler budget: %v", err)
	}
}

// It is bounded, though. "Independent" must not become "unbounded" — the whole
// reason the original shared a deadline was to stop this pass hanging forever.
func TestReconcileBudgetIsBounded(t *testing.T) {
	ctx, cancel := reconcileBudget(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the reconciler context has no deadline; it can hang forever")
	}
	remaining := time.Until(dl)
	if remaining <= 0 || remaining > reconcileBudgetFor+time.Second {
		t.Errorf("deadline is %s away, want ~%s", remaining.Round(time.Second), reconcileBudgetFor)
	}
}

// A short parent must NOT shorten the budget. This is the precise regression: the
// old code produced 2s here, which is a working context that times out mid-scan.
func TestReconcileBudgetIgnoresTheParentDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelParent()
	ctx, cancel := reconcileBudget(parent)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline")
	}
	if got := time.Until(dl); got < reconcileBudgetFor-time.Second {
		t.Errorf("budget is only %s — the parent's deadline leaked into it", got.Round(time.Second))
	}
}

// Values are preserved. WithoutCancel drops cancellation, not the context's values;
// dropping them too would break anything the parent carries (a DB handle, a logger).
func TestReconcileBudgetKeepsParentValues(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "carried")
	ctx, cancel := reconcileBudget(parent)
	defer cancel()
	if got := ctx.Value(key{}); got != "carried" {
		t.Errorf("value lost across the budget: %v", got)
	}
}
