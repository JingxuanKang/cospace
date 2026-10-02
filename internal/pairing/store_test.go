package pairing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCreateRedeemRoundTripAndSingleUse(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "invites.json"))
	code, err := store.Create("alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`).MatchString(code) {
		t.Fatalf("code = %q, want four Crockford characters, dash, four characters", code)
	}

	space, err := store.Redeem(code)
	if err != nil {
		t.Fatal(err)
	}
	if space != "alpha" {
		t.Fatalf("space = %q, want alpha", space)
	}
	if _, err := store.Redeem(code); !errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("second Redeem error = %v, want ErrAlreadyUsed", err)
	}
}

func TestUnknownCodeDoesNotPenalizeInvite(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "invites.json"))
	code, err := store.Create("alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for range maxAttempts {
		if _, err := store.Redeem("ZZZZ-ZZZZ"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("wrong code error = %v, want ErrInvalidCode", err)
		}
	}
	if space, err := store.Redeem(code); err != nil || space != "alpha" {
		t.Fatalf("right code after unrelated failures = %q, %v", space, err)
	}
}

func TestInviteAtAttemptLimitIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invites.json")
	code := "K7M4-Q2PX"
	sum := sha256.Sum256([]byte(code))
	disk := diskStore{Invites: []invite{{
		Hash:      hex.EncodeToString(sum[:]),
		Space:     "alpha",
		ExpiresAt: time.Now().Add(time.Minute).UTC(),
		Attempts:  maxAttempts,
	}}}
	b, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path).Redeem(code); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("Redeem error = %v, want ErrTooManyAttempts", err)
	}
}

func TestInviteExpiry(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "invites.json"))
	code, err := store.Create("alpha", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := store.Redeem(code); !errors.Is(err, ErrExpired) {
		t.Fatalf("Redeem error = %v, want ErrExpired", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invites.json")
	code, err := NewStore(path).Create("persistent", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	space, err := NewStore(path).Redeem(code)
	if err != nil {
		t.Fatal(err)
	}
	if space != "persistent" {
		t.Fatalf("space = %q, want persistent", space)
	}
}

func TestDiskContainsHashOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invites.json")
	code, err := NewStore(path).Create("alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), code) || strings.Contains(string(b), strings.ReplaceAll(code, "-", "")) {
		t.Fatalf("invite file leaked plaintext code: %s", b)
	}
	sum := sha256.Sum256([]byte(code))
	if !strings.Contains(string(b), hex.EncodeToString(sum[:])) {
		t.Fatalf("invite file does not contain SHA-256 hash: %s", b)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLookupPeeksWithoutConsuming(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "invites.json"))
	code, err := store.Create("peekspace", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		space, expires, err := store.Lookup(code)
		if err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if space != "peekspace" || !expires.After(time.Now()) {
			t.Fatalf("lookup %d: got %q %v", i, space, expires)
		}
	}
	if space, err := store.Redeem(code); err != nil || space != "peekspace" {
		t.Fatalf("redeem after lookups: %q %v", space, err)
	}
	if _, _, err := store.Lookup(code); err == nil {
		t.Fatal("lookup after redeem should fail")
	}
	if _, _, err := store.Lookup("ZZZZ-ZZZZ"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("unknown code: %v", err)
	}
}
