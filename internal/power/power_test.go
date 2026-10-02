package power

import "testing"

func TestReconcileIdempotent(t *testing.T) {
	starts, stops := 0, 0
	k := &Keeper{Start: func() (func(), error) {
		starts++
		return func() { stops++ }, nil
	}}
	k.Reconcile(true)
	k.Reconcile(true)
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
	k.Reconcile(false)
	k.Reconcile(false)
	if stops != 1 {
		t.Fatalf("stops = %d, want 1", stops)
	}
	k.Reconcile(true)
	if starts != 2 {
		t.Fatalf("restart after stop: starts = %d, want 2", starts)
	}
}
