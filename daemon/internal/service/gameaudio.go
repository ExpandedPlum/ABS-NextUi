package service

import (
	"log"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
)

// GameAudioStatus is reported to the UI.
type GameAudioStatus struct {
	Enabled bool   `json:"enabled"` // setting: mute games while a book plays
	Muted   bool   `json:"muted"`   // games are silenced right now
	Output  string `json:"output"`  // where NextUI sends audio
	Error   string `json:"error,omitempty"`
}

func (s *Service) muteGames() bool { return s.St.GetSettings().GameVolume < 0 }

// bookDevice is the ALSA device for the book: the real output, around the
// default device that games are silenced through. It runs each time
// playback starts, so it silences games first (on USB or Bluetooth a running
// game has to let go of the output before the book can open it) and notes
// the output in use.
func (s *Service) bookDevice() string {
	if s.game != nil {
		if s.muteGames() && s.setGameMute(true) {
			if k := s.game.Sink().Kind; k == "usb" || k == "bluetooth" {
				time.Sleep(300 * time.Millisecond)
			}
		}
		sig := s.game.Sink().String()
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

// setGameMute silences games or gives NextUI its audio file back, and
// reports whether anything changed.
func (s *Service) setGameMute(on bool) bool {
	changed, err := s.game.SetMuted(on)
	s.gameMu.Lock()
	defer s.gameMu.Unlock()
	msg := ""
	if err != nil {
		msg = err.Error()
		if msg != s.gameErr {
			log.Printf("game audio: %v", err)
		}
	}
	s.gameErr = msg
	if err != nil || !changed {
		return false
	}
	if on {
		s.gameMutedAt = time.Now()
		log.Printf("game audio: games muted (output: %s)", s.game.Sink())
	} else {
		log.Printf("game audio: game sound restored")
	}
	return true
}

// reconcileGameAudio runs every tick: games are muted exactly while a book
// is playing (or held for an app switch).
func (s *Service) reconcileGameAudio(state player.State, now time.Time) {
	if s.game == nil {
		return
	}
	want := s.muteGames() && (state == player.StatePlaying || state == player.StateLoading)
	if !want {
		// Playback may have just started from another goroutine after this
		// tick's snapshot was taken; don't undo its mute.
		s.gameMu.Lock()
		recent := now.Sub(s.gameMutedAt) < time.Second
		s.gameMu.Unlock()
		if recent {
			return
		}
	}
	s.setGameMute(want)
}

// followSink reopens the book's output when NextUI's output changes (USB or
// Bluetooth audio plugged in or out), so the book moves to the new output
// instead of staying on the old one, or failing because it went away.
func (s *Service) followSink(state player.State, now time.Time) {
	if s.game == nil || now.Sub(s.sinkChecked) < time.Second {
		return
	}
	s.sinkChecked = now
	if state != player.StatePlaying && state != player.StateError {
		return
	}
	sig := s.game.Sink().String()
	s.sinkMu.Lock()
	changed := s.sinkSig != "" && sig != s.sinkSig // "" = no output opened yet
	s.sinkMu.Unlock()
	if !changed {
		return
	}
	log.Printf("audio output changed to %s; reopening", sig)
	if state == player.StateError {
		_ = s.Eng.Play()
		return
	}
	_ = s.Eng.Seek(s.Eng.Position())
}

// restoreGameAudio gives games their sound back (on exit).
func (s *Service) restoreGameAudio() {
	if s.game == nil {
		return
	}
	if _, err := s.game.SetMuted(false); err != nil {
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
		Enabled: s.muteGames(),
		Muted:   s.game.Muted(),
		Output:  s.game.Sink().String(),
		Error:   s.gameErr,
	}
}
