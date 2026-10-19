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
// it is installed, otherwise the default (which follows Bluetooth/USB audio).
func (s *Service) bookDevice() string {
	if dev := s.St.GetSettings().AudioDevice; dev != "" && dev != "default" {
		return dev
	}
	if s.game == nil {
		return "default"
	}
	return s.game.BookDevice()
}

// reconcileGameAudio runs every tick: keeps the routing installed while the
// feature is on, and turns games down only while a book is playing.
func (s *Service) reconcileGameAudio(state player.State, now time.Time) {
	if s.game == nil {
		return
	}
	level := s.St.GetSettings().GameVolume
	s.gameMu.Lock()
	defer s.gameMu.Unlock()
	if s.gameBusy || now.Before(s.gameRetry) {
		return
	}

	if level >= 0 {
		// Feature off: put games back to normal and remove our routing.
		if s.gameApplied != 0 || s.game.Installed() {
			s.gameBusy = true
			go func() {
				if s.game.Installed() {
					_ = s.game.SetDB(0)
				}
				s.game.Remove()
				s.gameMu.Lock()
				s.gameApplied, s.gameBusy, s.gameErr = 0, false, ""
				s.gameMu.Unlock()
			}()
		}
		return
	}

	// (Re)install periodically: NextUI deletes the file at boot and
	// audiomon removes it when Bluetooth/USB audio disconnects.
	if now.Sub(s.gameChecked) > 5*time.Second || s.gameChecked.IsZero() {
		s.gameChecked = now
		if !s.game.Installed() {
			s.gameBusy = true
			go func() {
				err := s.game.Install()
				s.gameMu.Lock()
				s.gameBusy = false
				s.gameApplied = 0 // a fresh control starts at full volume
				if err != nil {
					if s.gameErr != err.Error() {
						log.Printf("game audio: %v", err)
					}
					s.gameErr = err.Error()
				} else {
					s.gameErr = ""
					log.Printf("game audio: routing installed")
				}
				s.gameMu.Unlock()
			}()
			return
		}
	}
	if !s.game.Installed() {
		return
	}

	want := 0
	if (state == player.StatePlaying || state == player.StateLoading) && !s.gameLetIn {
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
