package keepalive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeJWT(t *testing.T, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp, "sub": "x"})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func TestExpiryParsers(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().Add(4 * time.Hour).Truncate(time.Second)

	claude := filepath.Join(dir, "claude.json")
	os.WriteFile(claude, []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","expiresAt":%d}}`, exp.UnixMilli())), 0o600)
	if got, err := AnthropicExpiry(claude); err != nil || !got.Equal(exp) {
		t.Fatalf("anthropic: %v %v", got, err)
	}

	codex := filepath.Join(dir, "codex.json")
	os.WriteFile(codex, []byte(fmt.Sprintf(`{"tokens":{"access_token":%q}}`, fakeJWT(t, exp.Unix()))), 0o600)
	if got, err := OpenAIExpiry(codex); err != nil || !got.Equal(exp) {
		t.Fatalf("openai: %v %v", got, err)
	}

	grok := filepath.Join(dir, "grok.json")
	os.WriteFile(grok, []byte(fmt.Sprintf(`{"https://x.ai::abc":{"key":%q}}`, fakeJWT(t, exp.Unix()))), 0o600)
	if got, err := XAIExpiry(grok); err != nil || !got.Equal(exp) {
		t.Fatalf("xai: %v %v", got, err)
	}

	if _, err := JWTExpiry("garbage"); err == nil {
		t.Fatal("garbage JWT accepted")
	}
	if _, err := AnthropicExpiry(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}

type fakeSource struct {
	exp       time.Time
	refreshed int
	advance   bool // whether Refresh moves the expiry forward
	fail      bool
}

func (f *fakeSource) source(name string) Source {
	return Source{
		Provider: name,
		Expiry:   func() (time.Time, error) { return f.exp, nil },
		Refresh: func(ctx context.Context) error {
			f.refreshed++
			if f.fail {
				return fmt.Errorf("boom")
			}
			if f.advance {
				f.exp = f.exp.Add(8 * time.Hour)
			}
			return nil
		},
	}
}

func newTestKeeper(sources ...Source) (*Keeper, *time.Time) {
	now := time.Now()
	k := &Keeper{
		Sources:   sources,
		Threshold: 30 * time.Minute,
		Cooldown:  10 * time.Minute,
		Now:       func() time.Time { return now },
		Sleep:     func(time.Duration) {},
	}
	return k, &now
}

func TestTickRefreshesOnlyNearExpiry(t *testing.T) {
	near := &fakeSource{exp: time.Now().Add(10 * time.Minute), advance: true}
	far := &fakeSource{exp: time.Now().Add(6 * time.Hour), advance: true}
	k, _ := newTestKeeper(near.source("anthropic"), far.source("openai"))
	k.Tick(context.Background())
	if near.refreshed != 1 {
		t.Fatalf("near-expiry source not refreshed: %d", near.refreshed)
	}
	if far.refreshed != 0 {
		t.Fatalf("fresh source pinged: %d", far.refreshed)
	}
}

func TestTickHonorsCooldownAndNeeded(t *testing.T) {
	stuck := &fakeSource{exp: time.Now().Add(5 * time.Minute), advance: false}
	gated := &fakeSource{exp: time.Now().Add(-time.Hour), advance: true}
	k, now := newTestKeeper(stuck.source("anthropic"), gated.source("xai"))
	k.Needed = func(p string) bool { return p != "xai" }

	k.Tick(context.Background())
	k.Tick(context.Background()) // within cooldown: no second attempt
	if stuck.refreshed != 1 {
		t.Fatalf("cooldown not honored: %d", stuck.refreshed)
	}
	if gated.refreshed != 0 {
		t.Fatalf("Needed gate ignored: %d", gated.refreshed)
	}

	*now = now.Add(11 * time.Minute) // past cooldown: retried
	k.Tick(context.Background())
	if stuck.refreshed != 2 {
		t.Fatalf("no retry after cooldown: %d", stuck.refreshed)
	}
}

func TestTickRetriesExpiredEveryTick(t *testing.T) {
	dead := &fakeSource{exp: time.Now().Add(-time.Minute), advance: false}
	k, now := newTestKeeper(dead.source("xai"))
	k.Tick(context.Background())
	*now = now.Add(5 * time.Minute) // one tick later: already-expired creds retry
	k.Tick(context.Background())
	if dead.refreshed != 2 {
		t.Fatalf("expired credential not retried on next tick: %d", dead.refreshed)
	}
}

func TestTickCooldownAppliesToFailures(t *testing.T) {
	failing := &fakeSource{exp: time.Now().Add(-time.Hour), fail: true}
	k, _ := newTestKeeper(failing.source("anthropic"))
	k.Tick(context.Background())
	k.Tick(context.Background())
	if failing.refreshed != 1 {
		t.Fatalf("failed ping retried inside cooldown: %d", failing.refreshed)
	}
}
