package service

import (
	"log"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
)

// GameAudioStatus is reported to the UI.
type GameAudioStatus struct {
	Enabled   bool   `json:"enabled"`   // setting turns games down
	Available bool   `json:"available"` // routing is installed
	Ducking   bool   `json:"ducking"`   // games are turned down right now
	LetIn     bool   `json:"letIn"`     // user toggled game sound back on
	Error     string `json:"error,omitempty"`
}

// bookDevice is the ALSA device for the book: around the game control when
// it is installed, otherwise the default (which follows Bluetooth audio). It
// runs each time playback starts, so it also notes the routing in use.
func (s *Service) bookDevice() string {
	if s.game != nil {
		sig := s.game.Signature()
		s.sinkMu.Lock()
		s.sinkSig = sig
		s.sinkMu.Unlock()
	}
	if dev := s.St.GetSettings().AudioDevice; dev != "" && dev != "default" {
		return dev
	}
	if s.game == nil {
		return "default"
	}
	return s.game.BookDevice()
}

// unknownLevel marks the game control's value as unknown (just installed, or
// left from earlier), so the wanted level is always set.
const unknownLevel = 1

// reconcileGameAudio runs every tick: keeps the routing in line with the
// setting and the connected audio, and turns games down only while a book is
// playing.
func (s *Service) reconcileGameAudio(state player.State, now time.Time) {
	if s.game == nil {
		return
	}
	level := s.St.GetSettings().GameVolume
	enabled := level < 0
	s.gameMu.Lock()
	defer s.gameMu.Unlock()
	if s.gameBusy || now.Before(s.gameRetry) {
		return
	}

	// Check every second: NextUI deletes the file at boot, and audiomon
	// rewrites or deletes it as USB/Bluetooth audio comes and goes.
	if now.Sub(s.gameChecked) >= time.Second {
		s.gameChecked = now
		if s.game.Pending(enabled) {
			s.gameBusy = true
			go func() {
				err := s.game.Install(enabled)
				s.gameMu.Lock()
				s.gameBusy = false
				s.gameApplied = unknownLevel
				if err != nil {
					s.gameRetry = time.Now().Add(5 * time.Second)
					if s.gameErr != err.Error() {
						log.Printf("game audio: %v", err)
					}
					s.gameErr = err.Error()
				} else {
					s.gameErr = ""
					if s.game.Installed() {
						log.Printf("game audio: routing installed (book plays to %s)", s.game.BookDevice())
					}
				}
				s.gameMu.Unlock()
			}()
			return
		}
	}
	if !s.game.Installed() {
		s.gameErr = ""
		if enabled {
			if err := s.game.Unavailable(); err != nil {
				s.gameErr = err.Error()
			}
		}
		return
	}

	want := 0
	if enabled && (state == player.StatePlaying || state == player.StateLoading) && !s.gameLetIn {
		want = level
	}
	if want == s.gameApplied {
		return
	}
	s.gameBusy = true
	go func() {
		err := s.game.SetDB(want)
		s.gameMu.Lock()
		s.gameBusy = false
		if err != nil {
			// Back off, and only log when the error changes.
			s.gameRetry = time.Now().Add(5 * time.Second)
			if s.gameErr != err.Error() {
				log.Printf("game audio: %v", err)
			}
			s.gameErr = err.Error()
		} else {
			s.gameApplied = want
			s.gameErr = ""
		}
		s.gameMu.Unlock()
	}()
}

// followSink reopens the book's output when the audio routing changes (USB
// or Bluetooth audio plugged in or out), so the book moves to the new output
// instead of staying on the old one, or failing because it went away.
func (s *Service) followSink(state player.State, now time.Time) {
	if s.game == nil || now.Sub(s.sinkChecked) < time.Second {
		return
	}
	s.sinkChecked = now
	if state != player.StatePlaying && state != player.StateError {
		return
	}
	s.gameMu.Lock()
	settling := s.gameBusy || s.game.Pending(s.St.GetSettings().GameVolume < 0)
	s.gameMu.Unlock()
	if settling {
		return // our routing is about to change; follow it once it has
	}
	sig := s.game.Signature()
	s.sinkMu.Lock()
	changed := sig != s.sinkSig
	s.sinkMu.Unlock()
	if !changed {
		return
	}
	log.Printf("audio output changed; reopening")
	if state == player.StateError {
		_ = s.Eng.Play()
		return
	}
	_ = s.Eng.Seek(s.Eng.Position())
}

// restoreGameAudio puts games back to full volume (on exit).
func (s *Service) restoreGameAudio() {
	if s.game == nil || !s.game.Installed() {
		return
	}
	if err := s.game.SetDB(0); err != nil {
		log.Printf("game audio: restore: %v", err)
	}
}

func (s *Service) gameAudioStatus() GameAudioStatus {
	if s.game == nil {
		return GameAudioStatus{}
	}
	s.gameMu.Lock()
	defer s.gameMu.Unlock()
	return GameAudioStatus{
		Enabled:   s.St.GetSettings().GameVolume < 0,
		Available: s.game.Installed(),
		Ducking:   s.gameApplied < 0,
		LetIn:     s.gameLetIn,
		Error:     s.gameErr,
	}
}
