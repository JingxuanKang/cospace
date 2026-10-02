package gateway

import "testing"

func TestQuotaTrackerSpendAndLimit(t *testing.T) {
	q := newQuotaTracker(nil)
	if q.spentUSD("r") != 0 {
		t.Fatal("fresh space should have zero spend")
	}
	q.add("r", 4.50)
	q.add("r", 0.50)
	if got := q.spentUSD("r"); !approx(got, 5) {
		t.Fatalf("spent = %v, want 5", got)
	}
	// A non-positive cost is ignored.
	q.add("r", -1)
	q.add("r", 0)
	if got := q.spentUSD("r"); !approx(got, 5) {
		t.Fatalf("spent after no-op adds = %v, want 5", got)
	}
	// usdLimit 0 = unlimited.
	if q.overLimit("r", 0) {
		t.Error("limit 0 should never be over")
	}
	// Under limit.
	if q.overLimit("r", 10) {
		t.Error("5 < 10 should not be over limit")
	}
	// At limit is over (>=).
	if !q.overLimit("r", 5) {
		t.Error("5 >= 5 should be over limit")
	}
	if !q.overLimit("r", 4.99) {
		t.Error("5 >= 4.99 should be over limit")
	}
}

func TestQuotaTrackerSeed(t *testing.T) {
	q := newQuotaTracker(map[string]float64{"old": 42})
	if got := q.spentUSD("old"); !approx(got, 42) {
		t.Fatalf("seeded spend = %v, want 42", got)
	}
	q.add("old", 8)
	if got := q.spentUSD("old"); !approx(got, 50) {
		t.Fatalf("seeded + added = %v, want 50", got)
	}
}

func TestQuotaTrackerConcurrency(t *testing.T) {
	q := newQuotaTracker(nil)
	// maxConcurrency 0 = unlimited: many enters all succeed.
	for i := 0; i < 100; i++ {
		if !q.enter("r", 0) {
			t.Fatalf("unlimited concurrency rejected at %d", i)
		}
	}
	// A capped space admits up to the cap, then refuses.
	if !q.enter("c", 2) {
		t.Fatal("first slot should be granted")
	}
	if !q.enter("c", 2) {
		t.Fatal("second slot should be granted")
	}
	if q.enter("c", 2) {
		t.Fatal("third slot should be refused at cap 2")
	}
	// Releasing a slot lets the next in.
	q.leave("c")
	if !q.enter("c", 2) {
		t.Fatal("slot should be free after leave")
	}
	// leave never underflows below zero.
	q.leave("c")
	q.leave("c")
	q.leave("c")
	if !q.enter("c", 1) {
		t.Fatal("after draining, one slot at cap 1 should be grantable")
	}
}
