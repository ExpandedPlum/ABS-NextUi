package gameaudio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) (*Router, *[]string) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "asound.conf")
	_ = os.WriteFile(sys, []byte("pcm.!default {}\npcm.Playback {}\npcm.Capture { card audiocodec }\n"), 0o644)
	r := New(dir)
	r.SystemConf = sys
	var calls []string
	r.Run = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	return r, &calls
}

func TestInstallSetRemove(t *testing.T) {
	r, calls := setup(t)
	if r.BookDevice() != "default" {
		t.Fatal("book should use default before install")
	}
	if err := r.Install(true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(r.Path)
	if !strings.HasPrefix(string(b), marker) || !strings.Contains(string(b), `slave.pcm "Playback"`) {
		t.Fatalf("unexpected config:\n%s", b)
	}
	if r.BookDevice() != BookDevice {
		t.Fatal("book should bypass the game control once installed")
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "aplay -q -D ABSGame") {
		t.Fatalf("expected aplay to create the control, got %v", *calls)
	}
	if r.Pending(true) {
		t.Fatal("nothing should be pending once installed")
	}
	_ = r.Install(true) // idempotent: no second aplay
	if len(*calls) != 1 {
		t.Fatalf("control created twice: %v", *calls)
	}
	_ = r.SetDB(Mute)
	if got := (*calls)[1]; got != "amixer -q -c audiocodec cset name=ABS Game Volume 0,0" {
		t.Fatalf("mute command %q", got)
	}
	// Turning the feature off removes the speaker routing.
	if !r.Pending(false) {
		t.Fatal("removal should be pending")
	}
	_ = r.Install(false)
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("our file was not removed")
	}
	if r.Pending(false) {
		t.Fatal("nothing to do with the feature off and no file")
	}
}

// audiomon's raw USB DAC file is replaced by a shared one, whether or not
// games are being turned down, and NextUI still detects a USB DAC.
func TestUSBTakeover(t *testing.T) {
	for _, speaker := range []bool{true, false} {
		r, calls := setup(t)
		usb := "pcm.!default {\n    type hw\n    card 1\n}\nctl.!default {\n    type hw\n    card 1\n}\n"
		_ = os.WriteFile(r.Path, []byte(usb), 0o644)
		if r.BookDevice() != "default" || !r.Pending(speaker) {
			t.Fatal("raw USB file should be pending takeover")
		}
		if err := r.Install(speaker); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(r.Path)
		conf := string(b)
		for _, want := range []string{"# sink: usb 1", "type dmix", "card 1", "type hw", `slave.pcm "ABSOut"`, `playback.pcm "ABSGame"`} {
			if !strings.Contains(conf, want) {
				t.Fatalf("missing %q in:\n%s", want, conf)
			}
		}
		if strings.Contains(conf, "bluealsa") {
			t.Fatal("NextUI would mistake this for Bluetooth")
		}
		if r.BookDevice() != USBBookDevice || !r.Installed() || r.Pending(speaker) {
			t.Fatalf("book device %q after takeover", r.BookDevice())
		}
		if len(*calls) != 1 {
			t.Fatalf("expected one aplay, got %v", *calls)
		}
		// Unplugging: audiomon deletes the file; the speaker comes back.
		_ = os.Remove(r.Path)
		if r.Pending(speaker) != speaker {
			t.Fatal("speaker routing should follow the setting after unplug")
		}
	}
}

// A missing control (service restarted with the file already present) is
// recreated when setting the volume fails.
func TestSetDBRecreatesControl(t *testing.T) {
	r, calls := setup(t)
	failOnce := true
	r.Run = func(name string, args ...string) error {
		*calls = append(*calls, name)
		if name == "amixer" && failOnce {
			failOnce = false
			return os.ErrNotExist
		}
		return nil
	}
	if err := r.SetDB(Mute); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, ","); got != "amixer,aplay,amixer" {
		t.Fatalf("calls %s", got)
	}
}

func TestForeignFileUntouched(t *testing.T) {
	r, _ := setup(t)
	bt := "pcm.!default {\n type plug\n slave.pcm { type bluealsa } }\n"
	_ = os.WriteFile(r.Path, []byte(bt), 0o644)
	if r.Pending(true) {
		t.Fatal("Bluetooth file must not be pending")
	}
	if err := r.Install(true); err != ErrForeign {
		t.Fatalf("want ErrForeign, got %v", err)
	}
	if r.Unavailable() != ErrForeign {
		t.Fatal("Bluetooth should be reported")
	}
	_ = r.Install(false)
	b, _ := os.ReadFile(r.Path)
	if string(b) != bt {
		t.Fatal("Bluetooth config was modified")
	}
	if r.BookDevice() != "default" {
		t.Fatal("book must follow the default device (Bluetooth)")
	}
}

func TestUnsupported(t *testing.T) {
	r, _ := setup(t)
	_ = os.WriteFile(r.SystemConf, []byte("pcm.!default { type hw card 0 }\n"), 0o644)
	if err := r.Install(true); err != ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestRaw(t *testing.T) {
	for db, want := range map[int]int{0: 255, Mute: 0, -51: 1, -12: 195, -30: 105} {
		if got := Raw(db); got != want {
			t.Errorf("Raw(%d) = %d, want %d", db, got, want)
		}
	}
}
