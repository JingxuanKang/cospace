// Package imagesync makes sure the space base image is in the local store.
//
// A fresh host has never built anything: on first start the daemon pulls the
// prebuilt image in the background and the console shows its progress, so
// creating the first space never means waiting on a silent multi-minute pull.
package imagesync

import (
	"log"
	"sync"
	"time"

	"github.com/JingxuanKang/cospace/internal/container"
)

// Status is what the console shows about the base image.
type Status struct {
	Ref     string  `json:"ref"`
	State   string  `json:"state"` // checking | pulling | ready | error
	Phase   string  `json:"phase,omitempty"`
	Percent int     `json:"percent"`
	DoneMB  float64 `json:"done_mb,omitempty"`
	TotalMB float64 `json:"total_mb,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// Syncer pulls Ref once if it is missing, retrying until it lands.
type Syncer struct {
	Ref    string
	Exists func(ref string) (bool, error)
	Pull   func(ref string, report func(container.PullProgress)) error
	Retry  time.Duration // wait between failed attempts; defaults to 30s
	Log    *log.Logger

	mu sync.Mutex
	st Status
}

// Start checks for the image and, when it is missing, pulls it in the background.
func (s *Syncer) Start() {
	s.set(Status{Ref: s.Ref, State: "checking"})
	go s.loop()
}

// Status returns a snapshot for the console.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Ref == "" {
		return Status{Ref: s.Ref, State: "checking"}
	}
	return s.st
}

// Ready reports whether spaces can be created from the image now.
func (s *Syncer) Ready() bool { return s.Status().State == "ready" }

func (s *Syncer) loop() {
	retry := s.Retry
	if retry <= 0 {
		retry = 30 * time.Second
	}
	for {
		if s.attempt() {
			return
		}
		time.Sleep(retry)
	}
}

// attempt returns true once the image is present.
func (s *Syncer) attempt() bool {
	ok, err := s.Exists(s.Ref)
	if err != nil {
		s.fail(err)
		return false
	}
	if ok {
		s.set(Status{Ref: s.Ref, State: "ready", Percent: 100})
		return true
	}
	s.logf("image: pulling %s (first run only)", s.Ref)
	s.set(Status{Ref: s.Ref, State: "pulling", Phase: "fetching"})
	err = s.Pull(s.Ref, func(p container.PullProgress) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.st.Phase = p.Phase
		if p.Percent >= 0 {
			s.st.Percent = p.Percent
		}
		if p.TotalMB > 0 {
			s.st.DoneMB, s.st.TotalMB = p.DoneMB, p.TotalMB
		}
	})
	if err != nil {
		s.fail(err)
		return false
	}
	s.logf("image: %s ready", s.Ref)
	s.set(Status{Ref: s.Ref, State: "ready", Percent: 100})
	return true
}

func (s *Syncer) fail(err error) {
	s.logf("image: %v (retrying)", err)
	s.set(Status{Ref: s.Ref, State: "error", Error: err.Error()})
}

func (s *Syncer) set(st Status) {
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
}

func (s *Syncer) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}
