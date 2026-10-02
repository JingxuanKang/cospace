// Package usage persists per-space gateway usage as JSONL and aggregates it
// for the console. Token counts are estimates parsed from provider responses;
// request counts are exact.
//
// The JSONL file is the durable record; the aggregates the console and the
// quota seed need are kept in memory, keyed by space and day, built once at
// Open and advanced on every append. The file is only re-read for the rare
// query whose lower bound falls inside a day (a space recreated under the
// same name on the same day), so the per-request append never waits on a
// full-file scan and the console's polling cost does not grow with history.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type Row struct {
	TS        time.Time `json:"ts"`
	Space     string    `json:"space"`
	Provider  string    `json:"provider"`
	Status    int       `json:"status,omitempty"`
	TokensIn  int       `json:"tokens_in,omitempty"`
	TokensOut int       `json:"tokens_out,omitempty"`
	Requests  int       `json:"requests,omitempty"`
	Model     string    `json:"model,omitempty"`
	CostUSD   float64   `json:"cost_usd,omitempty"`
}

type Day struct {
	Date      string `json:"date"`
	Requests  int    `json:"requests"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
}

// dayAgg is one space's totals for one UTC day plus the time span of the
// rows behind it, so a query bounded inside the day can tell it must look
// at rows rather than the aggregate.
type dayAgg struct {
	Day
	cost        float64
	first, last time.Time
}

type Store struct {
	mu   sync.Mutex
	path string
	f    *os.File
	now  func() time.Time
	// Logf reports append failures (disk full, closed handle); nil = silent.
	Logf     func(format string, v ...any)
	lastWarn time.Time
	days     map[string]map[string]*dayAgg // space -> date -> totals
}

const dateLayout = "2006-01-02"

func Open(path string) (*Store, error) {
	migrateLegacyKeys(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, f: f, now: time.Now, days: map[string]map[string]*dayAgg{}}
	// Build the aggregates once; a damaged tail line is skipped, not fatal.
	if err := s.scanFile(func(r Row) { s.absorb(r) }); err != nil {
		return nil, fmt.Errorf("read usage history: %w", err)
	}
	return s, nil
}

// migrateLegacyKeys rewrites records written before the room→space rename so
// the whole file reads under the current schema. Best-effort and idempotent;
// runs before the store opens its append handle.
func migrateLegacyKeys(path string) {
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(b, []byte(`"room":`)) {
		return
	}
	out := bytes.ReplaceAll(b, []byte(`"room":`), []byte(`"space":`))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// absorb folds one row into the in-memory aggregates. Caller holds s.mu.
func (s *Store) absorb(r Row) {
	if r.Space == "" {
		return
	}
	date := r.TS.UTC().Format(dateLayout)
	bySpace := s.days[r.Space]
	if bySpace == nil {
		bySpace = map[string]*dayAgg{}
		s.days[r.Space] = bySpace
	}
	d := bySpace[date]
	if d == nil {
		d = &dayAgg{Day: Day{Date: date}, first: r.TS, last: r.TS}
		bySpace[date] = d
	}
	d.Requests += r.Requests
	d.TokensIn += r.TokensIn
	d.TokensOut += r.TokensOut
	d.cost += r.CostUSD
	if r.TS.Before(d.first) {
		d.first = r.TS
	}
	if r.TS.After(d.last) {
		d.last = r.TS
	}
}

func (s *Store) append(r Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.TS = s.now().UTC()
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		// Metering that silently vanishes would loosen every spend cap on the
		// next restart; say so, but not once per request.
		if s.Logf != nil && time.Since(s.lastWarn) > time.Minute {
			s.lastWarn = time.Now()
			s.Logf("usage: cannot append to %s: %v (quota seeds will be low until this is fixed)", s.path, err)
		}
	}
	s.absorb(r)
}

// Record implements gateway.UsageSink.
func (s *Store) Record(space, provider string, status int) {
	s.append(Row{Space: space, Provider: provider, Status: status, Requests: 1})
}

// RecordTokens implements gateway.TokenSink.
func (s *Store) RecordTokens(space, provider string, in, out int) {
	s.append(Row{Space: space, Provider: provider, TokensIn: in, TokensOut: out})
}

// RecordCost implements gateway.CostSink: it persists a request's token counts
// alongside the model that served it and its equivalent-dollar cost.
func (s *Store) RecordCost(space, provider, model string, in, out int, cost float64) {
	s.append(Row{Space: space, Provider: provider, Model: model, TokensIn: in, TokensOut: out, CostUSD: cost})
}

// SpentBySpace sums each space's cumulative equivalent-dollar spend across all
// history, used to seed the gateway's quota tracker at startup so limits
// survive a restart.
func (s *Store) SpentBySpace() (map[string]float64, error) {
	return s.SpentBySpaceSince(nil)
}

// SpentBySpaceSince sums spend for the current generation of each space. A space
// name may be reused after deletion, so rows older than that space's creation
// time must not seed the replacement space's quota.
func (s *Store) SpentBySpaceSince(since map[string]time.Time) (map[string]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]float64{}
	for space, bySpace := range s.days {
		var start time.Time
		if since != nil {
			var ok bool
			if start, ok = since[space]; !ok {
				continue
			}
		}
		total, straddle := 0.0, false
		for _, d := range bySpace {
			switch {
			case d.last.Before(start):
			case !d.first.Before(start):
				total += d.cost
			default:
				straddle = true
			}
		}
		if straddle {
			total = 0
			if err := s.scanFile(func(r Row) {
				if r.Space == space && !r.TS.Before(start) {
					total += r.CostUSD
				}
			}); err != nil {
				return nil, fmt.Errorf("aggregate spend: %w", err)
			}
		}
		if total != 0 {
			out[space] = total
		}
	}
	return out, nil
}

// scanFile replays every row on disk. Caller holds s.mu.
func (s *Store) scanFile(fn func(Row)) error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Row
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			fn(r)
		}
	}
	return sc.Err()
}

// Daily aggregates the last n days for one space, oldest first, days with no
// traffic omitted.
func (s *Store) Daily(space string, n int) ([]Day, error) {
	return s.DailySince(space, n, time.Time{})
}

// DailySince is Daily scoped to the current space generation. The explicit
// lower bound prevents a deleted space's history leaking into a new space that
// reuses the same human-friendly name.
func (s *Store) DailySince(space string, n int, since time.Time) ([]Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().UTC().AddDate(0, 0, -n)
	if since.After(cutoff) {
		cutoff = since
	}
	var out []Day
	var straddle *dayAgg
	for _, d := range s.days[space] {
		switch {
		case d.last.Before(cutoff):
		case !d.first.Before(cutoff):
			out = append(out, d.Day)
		default:
			straddle = d
		}
	}
	if straddle != nil {
		partial := Day{Date: straddle.Date}
		if err := s.scanFile(func(r Row) {
			if r.Space == space && !r.TS.Before(cutoff) && r.TS.UTC().Format(dateLayout) == straddle.Date {
				partial.Requests += r.Requests
				partial.TokensIn += r.TokensIn
				partial.TokensOut += r.TokensOut
			}
		}); err != nil {
			return nil, err
		}
		out = append(out, partial)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// TodayBySpace returns today's totals keyed by space.
func (s *Store) TodayBySpace() (map[string]Day, error) {
	return s.TodayBySpaceSince(nil)
}

// TodayBySpaceSince returns current-generation totals for every space in since.
func (s *Store) TodayBySpaceSince(since map[string]time.Time) (map[string]Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today := s.now().UTC().Format(dateLayout)
	out := map[string]Day{}
	for space, bySpace := range s.days {
		var start time.Time
		if since != nil {
			var ok bool
			if start, ok = since[space]; !ok {
				continue
			}
		}
		d := bySpace[today]
		if d == nil || d.last.Before(start) {
			continue
		}
		if !d.first.Before(start) {
			out[space] = d.Day
			continue
		}
		partial := Day{Date: today}
		if err := s.scanFile(func(r Row) {
			if r.Space == space && !r.TS.Before(start) && r.TS.UTC().Format(dateLayout) == today {
				partial.Requests += r.Requests
				partial.TokensIn += r.TokensIn
				partial.TokensOut += r.TokensOut
			}
		}); err != nil {
			return nil, fmt.Errorf("aggregate usage: %w", err)
		}
		out[space] = partial
	}
	return out, nil
}
