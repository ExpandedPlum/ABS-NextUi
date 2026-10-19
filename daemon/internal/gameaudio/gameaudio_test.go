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
	if err := r.Install(); err != nil {
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
	_ = r.Install() // idempotent: no second aplay
	if len(*calls) != 1 {
		t.Fatalf("control created twice: %v", *calls)
	}
	_ = r.SetDB(Mute)
	if got := (*calls)[1]; got != "amixer -q -c audiocodec cset name=ABS Game Volume 0,0" {
		t.Fatalf("mute command %q", got)
	}
	r.Remove()
	if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
		t.Fatal("our file was not removed")
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
	if err := r.Install(); err != ErrForeign {
		t.Fatalf("want ErrForeign, got %v", err)
	}
	r.Remove()
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
	if err := r.Install(); err != ErrUnsupported {
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
