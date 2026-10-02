package imagesync

import (
	"errors"
	"testing"
	"time"

	"github.com/JingxuanKang/cospace/internal/container"
)

func waitState(t *testing.T, s *Syncer, want string) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.Status(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state %q never reached; last %+v", want, s.Status())
	return Status{}
}

func TestPresentImageIsReadyWithoutPull(t *testing.T) {
	s := &Syncer{Ref: "img", Exists: func(string) (bool, error) { return true, nil },
		Pull: func(string, func(container.PullProgress)) error { t.Fatal("pulled a present image"); return nil }}
	s.Start()
	waitState(t, s, "ready")
}

func TestMissingImageIsPulledWithProgress(t *testing.T) {
	release := make(chan struct{})
	pulled := false
	s := &Syncer{Ref: "img",
		Exists: func(string) (bool, error) { return pulled, nil },
		Pull: func(_ string, report func(container.PullProgress)) error {
			report(container.PullProgress{Phase: "fetching", Percent: 40, DoneMB: 200, TotalMB: 500})
			<-release
			pulled = true
			return nil
		}}
	s.Start()
	deadline := time.Now().Add(2 * time.Second)
	for s.Status().Percent != 40 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	st := s.Status()
	if st.State != "pulling" || st.Percent != 40 || st.TotalMB != 500 {
		t.Fatalf("mid-pull status = %+v", st)
	}
	if s.Ready() {
		t.Fatal("ready before the pull finished")
	}
	close(release)
	waitState(t, s, "ready")
}

func TestFailedPullRetries(t *testing.T) {
	attempts := 0
	s := &Syncer{Ref: "img", Retry: time.Millisecond,
		Exists: func(string) (bool, error) { return false, nil },
		Pull: func(string, func(container.PullProgress)) error {
			attempts++
			if attempts < 3 {
				return errors.New("network down")
			}
			return nil
		}}
	s.Start()
	waitState(t, s, "ready")
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}
