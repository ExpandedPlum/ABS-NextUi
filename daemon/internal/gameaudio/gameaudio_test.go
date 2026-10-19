package gameaudio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	btFile = "defaults.bluealsa.device \"AA:BB:CC:DD:EE:FF\"\n\npcm.!default {\n    type plug\n    slave.pcm {\n        type bluealsa\n" +
		"        device \"AA:BB:CC:DD:EE:FF\"\n        profile \"a2dp\"\n        delay 0\n    }\n}\nctl.!default {\n    type bluealsa\n}\n"
)

func setup(t *testing.T) *Router {
	dir := t.TempDir()
	sys := filepath.Join(dir, "asound.conf")
	_ = os.WriteFile(sys, []byte("pcm.!default {}\npcm.Playback {}\npcm.Capture { card audiocodec }\n"), 0o644)
	r := New(dir)
	r.SystemConf = sys
	return r
}

func read(t *testing.T, r *Router) string {
	b, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What NextUI's detect_audio_device_type() would make of the file.
func nextuiSees(conf string) string {
	for _, line := range strings.Split(conf, "\n") {
		if strings.Contains(line, "type bluealsa") || strings.Contains(line, "defaults.bluealsa.device") {
			return "bluetooth"
		}
		if strings.Contains(line, "type hw") {
			return "usb"
		}
	}
	return "default"
}

func TestSpeaker(t *testing.T) {
	r := setup(t)
	if r.Sink().Kind != "speaker" || r.BookDevice() != "Playback" || r.Muted() {
		t.Fatal("unexpected initial state")
	}
	if changed, err := r.SetMuted(true); err != nil || !changed {
		t.Fatalf("mute: %v %v", changed, err)
	}
	conf := read(t, r)
	if !strings.Contains(conf, `playback.pcm "ABSSilent"`) || strings.Contains(conf, origPrefix) {
		t.Fatalf("unexpected config:\n%s", conf)
	}
	if !r.Muted() || r.Sink().Kind != "speaker" || r.BookDevice() != "Playback" {
		t.Fatal("muted state wrong")
	}
	if changed, _ := r.SetMuted(true); changed {
		t.Fatal("muting twice rewrote the file")
	}
	if changed, _ := r.SetMuted(false); !changed {
		t.Fatal("unmute did nothing")
	}
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("speaker unmute should leave no file")
	}
}

func TestUSBRestoresAudiomonFile(t *testing.T) {
	r := setup(t)
	usb := audiomonUSB("1")
	_ = os.WriteFile(r.Path, []byte(usb), 0o644)
	if k := r.Sink(); k.Kind != "usb" || k.ID != "1" || r.BookDevice() != "plughw:1,0" {
		t.Fatalf("sink %v, book %s", k, r.BookDevice())
	}
	_, _ = r.SetMuted(true)
	conf := read(t, r)
	if r.BookDevice() != "plughw:1,0" || !strings.Contains(conf, "card 1") {
		t.Fatalf("muted USB config:\n%s", conf)
	}
	if nextuiSees(conf) == "bluetooth" {
		t.Fatal("emulators would bypass the silent device")
	}
	_, _ = r.SetMuted(false)
	if read(t, r) != usb {
		t.Fatal("audiomon's file not restored exactly")
	}
}

func TestBluetooth(t *testing.T) {
	r := setup(t)
	_ = os.WriteFile(r.Path, []byte(btFile), 0o644)
	want := "bluealsa:DEV=AA:BB:CC:DD:EE:FF,PROFILE=a2dp"
	if k := r.Sink(); k.Kind != "bluetooth" || r.BookDevice() != want {
		t.Fatalf("sink %v, book %s", k, r.BookDevice())
	}
	_, _ = r.SetMuted(true)
	conf := read(t, r)
	if got := nextuiSees(conf); got != "default" {
		t.Fatalf("NextUI sees %s; emulators would open bluealsa directly:\n%s", got, conf)
	}
	if !strings.Contains(conf, `device "AA:BB:CC:DD:EE:FF"`) || r.BookDevice() != want {
		t.Fatal("Bluetooth address lost while muted")
	}
	_, _ = r.SetMuted(false)
	if read(t, r) != btFile {
		t.Fatal("Bluetooth file not restored exactly")
	}
}

// audiomon rewrites or deletes the file while a book plays: that becomes
// the new original, and games stay muted.
func TestAudiomonChangesWhileMuted(t *testing.T) {
	r := setup(t)
	_, _ = r.SetMuted(true)
	_ = os.WriteFile(r.Path, []byte(btFile), 0o644) // Bluetooth connects
	if r.Muted() || r.Sink().Kind != "bluetooth" {
		t.Fatal("should see Bluetooth, unmuted")
	}
	if changed, _ := r.SetMuted(true); !changed || r.Sink().Kind != "bluetooth" {
		t.Fatal("should re-mute, keeping Bluetooth as the original")
	}
	_ = os.Remove(r.Path) // disconnects
	if r.Sink().Kind != "speaker" {
		t.Fatal("should be back on the speaker")
	}
	_, _ = r.SetMuted(true)
	_, _ = r.SetMuted(false)
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("speaker unmute should leave no file")
	}
}

// Files from older versions count as ours.
func TestLegacyFiles(t *testing.T) {
	r := setup(t)
	_ = os.WriteFile(r.Path, []byte(markerPrefix+": game volume control.\npcm.!default {}\n"), 0o644)
	if !r.Muted() || r.Sink().Kind != "speaker" {
		t.Fatal("old speaker routing not recognized")
	}
	_, _ = r.SetMuted(false)
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("old file not removed")
	}
	_ = os.WriteFile(r.Path, []byte(markerPrefix+": game volume control.\n# sink: usb 2\n"), 0o644)
	if k := r.Sink(); k.Kind != "usb" || k.ID != "2" {
		t.Fatalf("old USB file: %v", k)
	}
	_, _ = r.SetMuted(false)
	if read(t, r) != audiomonUSB("2") {
		t.Fatal("old USB file not turned back into audiomon's")
	}
}

func TestUnknownAndUnsupported(t *testing.T) {
	r := setup(t)
	other := "pcm.!default { type plug slave.pcm \"something\" }\n"
	_ = os.WriteFile(r.Path, []byte(other), 0o644)
	if _, err := r.SetMuted(true); err != ErrUnknown || read(t, r) != other {
		t.Fatalf("unknown file must be left alone, got %v", err)
	}
	if r.BookDevice() != "default" {
		t.Fatal("book should use the default device")
	}

	r = setup(t)
	_ = os.WriteFile(r.SystemConf, []byte("pcm.!default { type hw card 0 }\n"), 0o644)
	if _, err := r.SetMuted(true); err != ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}
