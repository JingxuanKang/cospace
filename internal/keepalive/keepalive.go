// Package keepalive keeps the host's sponsor credentials fresh so spaces keep
// working while the host is away. Provider access tokens live for hours and
// the vendor CLIs only renew them when they actually run — a host who doesn't
// touch claude overnight strands every space on a dead token. The Keeper
// watches each credential's expiry and, just before it goes stale, runs the
// cheapest possible CLI call so the vendor CLI renews and persists the
// credential itself. No OAuth logic lives here: reimplementing refresh could
// race the CLI's own rotation and log the host out.
package keepalive

import (
	"context"
	"time"
)

// Source is one provider credential the Keeper watches.
type Source struct {
	Provider string
	// Expiry reads the credential's current expiry from disk.
	Expiry func() (time.Time, error)
	// Refresh performs the minimal CLI call whose side effect is the vendor
	// CLI renewing its credential file.
	Refresh func(ctx context.Context) error
}

type Keeper struct {
	Sources []Source
	// Threshold refreshes credentials that expire within this window.
	Threshold time.Duration
	// Cooldown is the minimum wait between refresh attempts per provider, so
	// a ping that doesn't advance the expiry doesn't turn into a hot loop.
	Cooldown time.Duration
	// Needed reports whether a provider is worth keeping fresh (sponsor gate
	// on and at least one space enables it). nil means always.
	Needed func(provider string) bool
	Logf   func(format string, v ...any)
	Now    func() time.Time      // test hook; defaults to time.Now
	Sleep  func(d time.Duration) // test hook; defaults to time.Sleep

	last map[string]time.Time
}

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Keeper) sleep(d time.Duration) {
	if k.Sleep != nil {
		k.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (k *Keeper) logf(format string, v ...any) {
	if k.Logf != nil {
		k.Logf(format, v...)
	}
}

// Tick checks every source once and pings those about to expire. An expiry
// that fails to read is skipped silently — a provider the host never logged
// into simply has no file.
func (k *Keeper) Tick(ctx context.Context) {
	if k.last == nil {
		k.last = map[string]time.Time{}
	}
	for _, s := range k.Sources {
		if k.Needed != nil && !k.Needed(s.Provider) {
			continue
		}
		exp, err := s.Expiry()
		if err != nil {
			continue
		}
		now := k.now()
		if exp.Sub(now) > k.Threshold {
			continue
		}
		// Some CLIs (grok, observed) ignore a ping while the token is still
		// valid and only renew once it is dead. Pre-expiry pings back off by
		// the full cooldown; once expired, retry every tick so the outage is
		// bounded by the tick interval, not the cooldown.
		cool := k.Cooldown
		if exp.Before(now) {
			cool = 2 * time.Minute
		}
		if last, ok := k.last[s.Provider]; ok && now.Sub(last) < cool {
			continue
		}
		k.last[s.Provider] = now
		k.logf("keepalive: %s credential expires %s — pinging the CLI to refresh", s.Provider, exp.Format(time.RFC3339))
		if err := s.Refresh(ctx); err != nil {
			k.logf("keepalive: %s ping failed: %v", s.Provider, err)
			continue
		}
		// The CLI may persist the renewed credential shortly after its
		// stdout closes, so give the file a moment before judging.
		advanced := false
		for i := 0; i < 6; i++ {
			if newExp, err := s.Expiry(); err == nil && newExp.After(exp) {
				advanced = true
				break
			}
			k.sleep(5 * time.Second)
		}
		if advanced {
			k.logf("keepalive: %s credential refreshed", s.Provider)
		} else {
			// Some CLIs only renew once the token is actually dead; the
			// next tick after expiry will catch it.
			k.logf("keepalive: %s ping ran but expiry did not advance; retrying after cooldown", s.Provider)
		}
	}
}

// Run ticks immediately and then on every interval until ctx ends.
func (k *Keeper) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		k.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
