// Package gameaudio lets the daemon turn game (emulator) audio down while a
// book plays.
//
// TrimUI's /etc/asound.conf routes the ALSA default device through
// "Playback" (a softvol master in front of a dmix mixer). NextUI runs every
// program with HOME=$USERDATA_PATH, so a $HOME/.asoundrc can put one more
// softvol, "ABS Game Volume", in front of the default device that games use:
//
//	game → ABSGame (ABS Game Volume) → Playback (master) → dmix → speaker
//	book ───────────────────────────→ Playback (master) ↗
//
// The book plays to "Playback" directly so it isn't affected. NextUI's
// audiomon owns the same file while Bluetooth or USB audio is connected; a
// file without our marker is never touched.
package gameaudio

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	marker     = "# Managed by Audiobookshelf.pak: game volume control."
	controlFmt = "ABS Game Volume"
	pcmName    = "ABSGame"
	// BookDevice is where the book plays when the router is installed.
	BookDevice = "Playback"
	minDB      = -51.0
	resolution = 256
	// Mute is the dB value meaning "silent".
	Mute = -99
)

var (
	ErrForeign     = errors.New("another audio setup is active (Bluetooth or USB audio)")
	ErrUnsupported = errors.New("this device's audio setup isn't supported")
)

type Router struct {
	Path       string // $USERDATA_PATH/.asoundrc
	SystemConf string // /etc/asound.conf
	Card       string
	// Run executes a command with HOME pointing at Path's directory, so ALSA
	// reads our .asoundrc. Replaceable in tests.
	Run func(name string, args ...string) error

	mu      sync.Mutex
	created bool // the softvol control exists on the card
}

func New(userdata string) *Router {
	r := &Router{
		Path:       filepath.Join(userdata, ".asoundrc"),
		SystemConf: "/etc/asound.conf",
		Card:       "audiocodec",
	}
	r.Run = func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(r.Path))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return r
}

func (r *Router) config() string {
	return marker + `
# Games use the default device, which passes through "ABS Game Volume".
# The audiobook plays to "Playback" directly. Deleted by NextUI at boot.
pcm.!default {
    type asym
    playback.pcm "` + pcmName + `"
    capture.pcm "Capture"
}

pcm.` + pcmName + ` {
    type plug
    slave.pcm {
        type softvol
        slave.pcm "Playback"
        control {
            name "` + controlFmt + `"
            card ` + r.Card + `
        }
        min_dB ` + fmt.Sprintf("%.1f", minDB) + `
        max_dB 0.0
        resolution ` + fmt.Sprint(resolution) + `
    }
}
`
}

// Supported reports whether the system config has the layout we chain onto.
func (r *Router) Supported() bool {
	b, err := os.ReadFile(r.SystemConf)
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "pcm.Playback") && strings.Contains(s, "pcm.Capture") && strings.Contains(s, r.Card)
}

// state: installed = our file is present; foreign = someone else's file is.
func (r *Router) state() (installed, foreign bool) {
	b, err := os.ReadFile(r.Path)
	if err != nil {
		return false, false
	}
	if strings.HasPrefix(string(b), marker) {
		return true, false
	}
	return false, len(strings.TrimSpace(string(b))) > 0
}

// Installed reports whether games are currently routed through our control.
func (r *Router) Installed() bool {
	inst, _ := r.state()
	return inst
}

// Install writes our .asoundrc (unless another one is in place) and makes
// sure the volume control exists so it can be set before a game starts.
func (r *Router) Install() error {
	if !r.Supported() {
		return ErrUnsupported
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, foreign := r.state()
	if foreign {
		return ErrForeign
	}
	if !inst {
		tmp := r.Path + ".tmp"
		if err := os.WriteFile(tmp, []byte(r.config()), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, r.Path); err != nil {
			return err
		}
		r.created = false
	}
	if !r.created {
		if err := r.createControl(); err != nil {
			return err
		}
		r.created = true
	}
	return nil
}

// createControl opens the ABSGame PCM briefly: softvol registers its mixer
// control on first open. The control can disappear (a service restart with
// the file already present, or the codec being reset), so this is also done
// whenever setting the volume fails.
func (r *Router) createControl() error {
	if err := r.Run("aplay", "-q", "-D", pcmName, "-f", "S16_LE", "-r", "48000", "-c", "2", "-d", "1", "/dev/zero"); err != nil {
		return fmt.Errorf("creating volume control: %w", err)
	}
	return nil
}

// Remove deletes our .asoundrc, leaving any other file alone.
func (r *Router) Remove() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inst, _ := r.state(); inst {
		_ = os.Remove(r.Path)
	}
}

// Raw converts a dB level to the control's raw value.
func Raw(db int) int {
	if db <= Mute {
		return 0
	}
	if db >= 0 {
		return resolution - 1
	}
	v := int((float64(db)-minDB)/(-minDB)*float64(resolution-1) + 0.5)
	if v < 1 {
		v = 1
	}
	return v
}

// SetDB sets game volume: 0 = unchanged, negative = quieter, Mute = silent.
// If the control is missing it is recreated and the set retried once.
func (r *Router) SetDB(db int) error {
	raw := Raw(db)
	set := func() error {
		return r.Run("amixer", "-q", "-c", r.Card, "cset", "name="+controlFmt, fmt.Sprintf("%d,%d", raw, raw))
	}
	if err := set(); err == nil {
		return nil
	}
	r.mu.Lock()
	err := r.createControl()
	r.created = err == nil
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return set()
}

// BookDevice returns the ALSA device the book should use: "Playback" while
// our routing is installed (so the book skips the game control), otherwise
// the default device (which may be Bluetooth or USB audio).
func (r *Router) BookDevice() string {
	if r.Installed() {
		return BookDevice
	}
	return "default"
}
