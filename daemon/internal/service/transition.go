package service

import (
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
)

// TransitionPause holds the book for d while NextUI switches apps, then
// resumes it where it stopped (no resume rewind, no sync).
//
// Every NextUI app sets the speaker to full volume for a moment when it
// starts (libmsettings InitSettings) before restoring the user's level,
// which is a loud blip with a book playing. NextUI's pre-/post-launch hooks
// call this around each launch so the book is silent through it. Calling it
// again while held restarts the wait.
func (s *Service) TransitionPause(d time.Duration) {
	s.mu.Lock()
	held := s.trans != nil
	if held {
		s.trans.Stop()
	}
	s.mu.Unlock()
	if !held {
		switch s.Eng.Snapshot().State {
		case player.StatePlaying, player.StateLoading:
		default:
			return
		}
		s.Eng.Pause()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		s.mu.Lock()
		mine := s.trans == t
		if mine {
			s.trans = nil
		}
		s.mu.Unlock()
		if mine {
			_ = s.Eng.Play()
		}
	})
	s.trans = t
}

// endTransition cancels a pending automatic resume; it reports whether the
// book was being held for an app switch (and so counts as playing).
func (s *Service) endTransition() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trans == nil {
		return false
	}
	s.trans.Stop()
	s.trans = nil
	return true
}

func (s *Service) inTransition() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trans != nil
}
