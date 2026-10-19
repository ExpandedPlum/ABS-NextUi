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
// The book plays to "Playback" directly so it isn't affected.
//
// NextUI's audiomon writes the same file while Bluetooth or USB audio is
// connected. Its USB file opens the DAC as a raw "hw" device, which only one
// program can use at a time and which does no format conversion, so the book
// can't play next to a game. That file is replaced with one that mixes onto
// the DAC instead, with the same game control in front:
//
//	game → ABSGame (ABS Game Volume) → ABSOut → ABSMix (dmix) → USB DAC
//	book ───────────────────────────→ ABSOut ↗
//
// The replacement keeps a "type hw" line so NextUI still sees a USB DAC.
// audiomon deletes the file on unplug. Bluetooth files are left alone:
// emulators open the "bluealsa" device directly, not the default.
package gameaudio

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	marker     = "# Managed by Audiobookshelf.pak: game volume control."
	controlFmt = "ABS Game Volume"
	pcmName    = "ABSGame"
	// BookDevice is where the book plays when the speaker routing is
	// installed; USBBookDevice when the USB routing is.
	BookDevice    = "Playback"
	USBBookDevice = "ABSOut"
	minDB         = -51.0
	resolution    = 256
	// Mute is the dB value meaning "silent".
	Mute = -99
)

var (
	ErrForeign     = errors.New("not available with Bluetooth audio")
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

// Kinds of .asoundrc.
const (
	kindNone       = iota // no file: the built-in speaker
	kindSpeaker           // ours, speaker routing
	kindUSB               // ours, USB DAC routing
	kindForeignUSB        // audiomon's USB DAC file
	kindForeign           // anything else (Bluetooth)
)

// audiomon's USB file: pcm.!default { type hw card N }.
var usbFile = regexp.MustCompile(`pcm\.!default\s*\{\s*type\s+hw\s+card\s+([A-Za-z0-9_]+)\s*\}`)
var usbHeader = regexp.MustCompile(`(?m)^# sink: usb ([A-Za-z0-9_]+)$`)

// gamePCM is the softvol in front of slave that games play through.
func (r *Router) gamePCM(slave string) string {
	return `pcm.` + pcmName + ` {
    type plug
    slave.pcm {
        type softvol
        slave.pcm "` + slave + `"
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

func (r *Router) speakerConfig() string {
	return marker + `
# sink: speaker
# Games use the default device, which passes through "ABS Game Volume".
# The audiobook plays to "Playback" directly. Deleted by NextUI at boot.
pcm.!default {
    type asym
    playback.pcm "` + pcmName + `"
    capture.pcm "Capture"
}

` + r.gamePCM("Playback")
}

func (r *Router) usbConfig(card string) string {
	return marker + `
# sink: usb ` + card + `
# USB audio (card ` + card + `), shared so the audiobook can play next to games.
# Games use the default device, which passes through "ABS Game Volume".
# The audiobook plays to "` + USBBookDevice + `". NextUI deletes this on unplug.
pcm.!default {
    type asym
    playback.pcm "` + pcmName + `"
    capture.pcm "plughw:` + card + `,0"
}

ctl.!default {
    type hw
    card ` + card + `
}

pcm.` + USBBookDevice + ` {
    type plug
    slave.pcm "ABSMix"
}

pcm.ABSMix {
    type dmix
    ipc_key 2469
    ipc_perm 0666
    slave {
        pcm {
            type hw
            card ` + card + `
            device 0
        }
        rate 48000
        channels 2
    }
}

` + r.gamePCM(USBBookDevice)
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

// state reads .asoundrc: its kind, and the USB card for the USB kinds.
func (r *Router) state() (kind int, card string) {
	b, err := os.ReadFile(r.Path)
	if err != nil {
		return kindNone, ""
	}
	s := string(b)
	if strings.HasPrefix(s, marker) {
		if m := usbHeader.FindStringSubmatch(s); m != nil {
			return kindUSB, m[1]
		}
		return kindSpeaker, ""
	}
	if !strings.Contains(s, "bluealsa") {
		if m := usbFile.FindStringSubmatch(s); m != nil {
			return kindForeignUSB, m[1]
		}
	}
	if strings.TrimSpace(s) == "" {
		return kindNone, ""
	}
	return kindForeign, ""
}

// Installed reports whether games are currently routed through our control.
func (r *Router) Installed() bool {
	k, _ := r.state()
	return k == kindSpeaker || k == kindUSB
}

// Signature identifies the current audio routing; it changes whenever the
// book's output should be reopened.
func (r *Router) Signature() string {
	b, _ := os.ReadFile(r.Path)
	return string(b)
}

// Unavailable says why games can't be routed through our control right now
// (Bluetooth audio, or an unsupported device), or nil.
func (r *Router) Unavailable() error {
	switch k, _ := r.state(); k {
	case kindForeign:
		return ErrForeign
	case kindNone:
		if !r.Supported() {
			return ErrUnsupported
		}
	}
	return nil
}

// Pending reports whether Install(speaker) has anything to do.
func (r *Router) Pending(speaker bool) bool {
	k, _ := r.state()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch k {
	case kindForeignUSB:
		return true
	case kindNone:
		return speaker && r.Supported()
	case kindSpeaker:
		return !speaker || !r.created
	case kindUSB:
		return !r.created
	}
	return false
}

// Install brings .asoundrc in line: a USB DAC is always shared (so the book
// can play at all), and the speaker routing is added when speaker is true and
// removed when it is false. It makes sure the volume control exists so it can
// be set before a game starts.
func (r *Router) Install(speaker bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, card := r.state()
	switch k {
	case kindForeign:
		return ErrForeign
	case kindForeignUSB:
		if err := r.write(r.usbConfig(card)); err != nil {
			return err
		}
	case kindNone:
		if !speaker {
			return nil
		}
		if !r.Supported() {
			return ErrUnsupported
		}
		if err := r.write(r.speakerConfig()); err != nil {
			return err
		}
	case kindSpeaker:
		if !speaker {
			_ = os.Remove(r.Path)
			return nil
		}
	}
	if !r.created {
		if err := r.createControl(); err != nil {
			return err
		}
		r.created = true
	}
	return nil
}

func (r *Router) write(conf string) error {
	tmp := r.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.Path); err != nil {
		return err
	}
	r.created = false
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

// BookDevice returns the ALSA device the book should use: one that skips the
// game control while our routing is installed, otherwise the default device
// (which may be Bluetooth audio).
func (r *Router) BookDevice() string {
	switch k, _ := r.state(); k {
	case kindSpeaker:
		return BookDevice
	case kindUSB:
		return USBBookDevice
	}
	return "default"
}
