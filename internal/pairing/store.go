// Package pairing persists short-lived, one-time invitation codes.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxAttempts = 5
	crockford   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var (
	ErrInvalidCode     = errors.New("invalid invite code")
	ErrExpired         = errors.New("invite expired")
	ErrTooManyAttempts = errors.New("too many invite attempts")
	ErrAlreadyUsed     = errors.New("invite already used")
)

type invite struct {
	Hash      string    `json:"hash"`
	Space     string    `json:"space"`
	ExpiresAt time.Time `json:"expiry"`
	Attempts  int       `json:"attempts"`
}

type diskStore struct {
	Invites []invite `json:"invites"`
}

// Store is safe for concurrent use within a process and across processes:
// every operation reloads the file under an advisory lock, so an invite
// created by the cospaced CLI is visible to an already-running serve, and a
// redeem racing a create cannot resurrect a consumed code or drop a new one.
type Store struct {
	path string

	mu   sync.Mutex
	used map[[sha256.Size]byte]struct{}
}

// flock holds the cross-process lock for one load→modify→save cycle. Callers
// hold s.mu as well. A lock that cannot be taken degrades to in-process
// safety rather than failing the operation.
func (s *Store) flock() func() {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return func() {}
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func NewStore(path string) *Store {
	return &Store{path: path, used: make(map[[sha256.Size]byte]struct{})}
}

func (s *Store) Create(space string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(space) == "" {
		return "", errors.New("invite space is required")
	}
	if ttl <= 0 {
		return "", errors.New("invite TTL must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.flock()()
	disk, err := s.load()
	if err != nil {
		return "", err
	}

	for {
		code, err := newCode(rand.Reader)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(code))
		hash := hex.EncodeToString(sum[:])
		duplicate := false
		for _, existing := range disk.Invites {
			if subtle.ConstantTimeCompare([]byte(existing.Hash), []byte(hash)) == 1 {
				duplicate = true
			}
		}
		if duplicate {
			continue
		}
		disk.Invites = append(disk.Invites, invite{
			Hash:      hash,
			Space:     space,
			ExpiresAt: time.Now().UTC().Add(ttl),
		})
		if err := s.save(disk); err != nil {
			return "", err
		}
		return code, nil
	}
}

// Lookup reports the space and expiry behind an outstanding code without
// consuming it or counting an attempt, so the public invite page can render
// instructions for a link that is opened more than once. Unknown, exhausted,
// and expired codes all come back as errors the caller should treat alike.
func (s *Store) Lookup(code string) (string, time.Time, error) {
	canonical := canonicalCode(code)
	sum := sha256.Sum256([]byte(canonical))
	wantHash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.used[sum]; ok {
		return "", time.Time{}, ErrAlreadyUsed
	}
	defer s.flock()()
	disk, err := s.load()
	if err != nil {
		return "", time.Time{}, err
	}
	match := -1
	for i := range disk.Invites {
		if subtle.ConstantTimeCompare([]byte(disk.Invites[i].Hash), []byte(wantHash)) == 1 {
			match = i
		}
	}
	if match < 0 {
		return "", time.Time{}, ErrInvalidCode
	}
	inv := disk.Invites[match]
	if inv.Attempts >= maxAttempts {
		return "", time.Time{}, ErrTooManyAttempts
	}
	if !time.Now().UTC().Before(inv.ExpiresAt) {
		return "", time.Time{}, ErrExpired
	}
	return inv.Space, inv.ExpiresAt, nil
}

// Canonical returns the display form of a code: uppercase, XXXX-XXXX.
func Canonical(code string) string { return canonicalCode(code) }

// Redeem validates and consumes code. Unknown codes deliberately do not alter
// any invite: with only a full-code hash on disk, they cannot safely be
// attributed to a particular invitation.
func (s *Store) Redeem(code string) (string, error) {
	canonical := canonicalCode(code)
	sum := sha256.Sum256([]byte(canonical))
	wantHash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.used[sum]; ok {
		return "", ErrAlreadyUsed
	}
	defer s.flock()()
	disk, err := s.load()
	if err != nil {
		return "", err
	}

	match := -1
	for i := range disk.Invites {
		if subtle.ConstantTimeCompare([]byte(disk.Invites[i].Hash), []byte(wantHash)) == 1 {
			match = i
		}
	}
	if match < 0 {
		return "", ErrInvalidCode
	}
	inv := &disk.Invites[match]
	if inv.Attempts >= maxAttempts {
		return "", ErrTooManyAttempts
	}
	if !time.Now().UTC().Before(inv.ExpiresAt) {
		inv.Attempts++
		if err := s.save(disk); err != nil {
			return "", err
		}
		return "", ErrExpired
	}

	space := inv.Space
	disk.Invites = append(disk.Invites[:match], disk.Invites[match+1:]...)
	if err := s.save(disk); err != nil {
		return "", err
	}
	s.used[sum] = struct{}{}
	return space, nil
}

func canonicalCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) == 8 && !strings.Contains(code, "-") {
		code = code[:4] + "-" + code[4:]
	}
	return code
}

func newCode(r io.Reader) (string, error) {
	var raw [5]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return "", fmt.Errorf("generate invite code: %w", err)
	}
	var b strings.Builder
	b.Grow(9)
	var bits uint64
	for _, v := range raw {
		bits = bits<<8 | uint64(v)
	}
	for i := 7; i >= 0; i-- {
		if i == 3 {
			b.WriteByte('-')
		}
		b.WriteByte(crockford[(bits>>uint(i*5))&31])
	}
	return b.String(), nil
}

func (s *Store) load() (diskStore, error) {
	var disk diskStore
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return disk, nil
	}
	if err != nil {
		return disk, fmt.Errorf("read invites: %w", err)
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		return disk, fmt.Errorf("parse invites: %w", err)
	}
	return disk, nil
}

func (s *Store) save(disk diskStore) error {
	b, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create invite directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".invites-*.tmp")
	if err != nil {
		return fmt.Errorf("create invite temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace invites: %w", err)
	}
	return nil
}
