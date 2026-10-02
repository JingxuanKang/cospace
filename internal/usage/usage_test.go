package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAndAggregate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	s.Record("acme", "anthropic", 200)
	s.RecordTokens("acme", "anthropic", 1200, 340)
	s.RecordTokens("acme", "anthropic", 800, 60)
	s.Record("thesis", "openai", 200)
	now = now.Add(24 * time.Hour)
	s.RecordTokens("acme", "anthropic", 500, 100)

	days, err := s.Daily("acme", 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("want 2 days, got %+v", days)
	}
	d0 := days[0]
	// Requests counts Record calls only; RecordTokens rows supplement the same request.
	if d0.Date != "2026-08-29" || d0.TokensIn != 2000 || d0.TokensOut != 400 || d0.Requests != 1 {
		t.Fatalf("day0 wrong: %+v", d0)
	}
	if days[1].Date != "2026-08-30" || days[1].TokensOut != 100 {
		t.Fatalf("day1 wrong: %+v", days[1])
	}

	// totals for all spaces
	tot, err := s.TodayBySpace()
	if err != nil {
		t.Fatal(err)
	}
	if tot["acme"].TokensOut != 100 {
		t.Fatalf("today acme: %+v", tot["acme"])
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	s, _ := Open(p)
	s.RecordTokens("acme", "anthropic", 10, 20)
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	days, err := s2.Daily("acme", 14)
	if err != nil || len(days) != 1 || days[0].TokensOut != 20 {
		t.Fatalf("reopen lost data: %+v err=%v", days, err)
	}
}

func TestRecordCostAndSpendBySpace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.RecordCost("acme", "anthropic", "claude-3-5-sonnet", 1000, 200, 1.25)
	s.RecordCost("acme", "openai", "gpt-5.6", 500, 100, 0.75)
	s.RecordCost("thesis", "xai", "grok-4", 300, 50, 0.40)
	// A plain token row (no cost) must not affect spend totals.
	s.RecordTokens("acme", "anthropic", 999, 999)

	spent, err := s.SpentBySpace()
	if err != nil {
		t.Fatal(err)
	}
	if got := spent["acme"]; got < 1.999 || got > 2.001 {
		t.Fatalf("acme spend = %v, want 2.00", got)
	}
	if got := spent["thesis"]; got < 0.399 || got > 0.401 {
		t.Fatalf("thesis spend = %v, want 0.40", got)
	}

	// Spend survives a reopen (seeds the quota tracker on restart).
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	spent2, err := s2.SpentBySpace()
	if err != nil {
		t.Fatal(err)
	}
	if got := spent2["acme"]; got < 1.999 || got > 2.001 {
		t.Fatalf("acme spend after reopen = %v, want 2.00", got)
	}
}

func TestGenerationCutoffExcludesReusedSpaceHistory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.RecordCost("acme", "anthropic", "old", 1000, 20, 1.5)
	s.Record("acme", "anthropic", 200)
	created := now.Add(time.Hour)
	now = created.Add(time.Minute)
	s.RecordCost("acme", "anthropic", "new", 30, 4, 0.25)
	s.Record("acme", "anthropic", 200)

	days, err := s.DailySince("acme", 14, created)
	if err != nil || len(days) != 1 || days[0].TokensIn != 30 || days[0].Requests != 1 {
		t.Fatalf("current-generation daily = %+v, err=%v", days, err)
	}
	today, err := s.TodayBySpaceSince(map[string]time.Time{"acme": created})
	if err != nil || today["acme"].TokensOut != 4 || today["acme"].Requests != 1 {
		t.Fatalf("current-generation today = %+v, err=%v", today, err)
	}
	spent, err := s.SpentBySpaceSince(map[string]time.Time{"acme": created})
	if err != nil || spent["acme"] != 0.25 {
		t.Fatalf("current-generation spend = %+v, err=%v", spent, err)
	}
}
