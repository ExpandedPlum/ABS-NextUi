// absd is the Audiobookshelf playback daemon for NextUI.
//
//	absd serve          run the daemon (normally started by launch.sh)
//	absd ctl <cmd> [json]  send a command to the running daemon, e.g.
//	                     absd ctl toggle, absd ctl presleep, absd ctl status
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/hotkeys"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/service"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func socketPath() string { return env("ABSD_SOCKET", "/tmp/absd.sock") }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: absd serve | absd ctl <command> [json]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve()
	case "ctl":
		os.Exit(ctl(os.Args[2:]))
	case "version":
		fmt.Println(service.Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func unixClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath())
		}},
	}
}

func ctl(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: absd ctl <command> [json-body]")
		return 2
	}
	path := "/" + strings.TrimPrefix(args[0], "/")
	var body io.Reader = bytes.NewReader(nil)
	if len(args) > 1 {
		body = strings.NewReader(args[1])
	}
	resp, err := unixClient(15*time.Second).Post("http://absd"+path, "application/json", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "absd not running:", err)
		return 1
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	fmt.Println(string(out))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func alreadyRunning() bool {
	resp, err := unixClient(2 * time.Second).Get("http://absd/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func defaultFFmpeg() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "ffmpeg")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "ffmpeg"
}

func defaultDataDir() string {
	if s := os.Getenv("SHARED_USERDATA_PATH"); s != "" {
		return filepath.Join(s, "Audiobookshelf")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".absd")
}

func serve() {
	if alreadyRunning() {
		fmt.Println("absd already running")
		return
	}
	// Only one daemon may own the socket and the audio device.
	lock, err := os.OpenFile(env("ABSD_LOCK", "/tmp/absd.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		defer lock.Close() // keeps the lock held (and the file reachable) until exit
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			fmt.Println("absd already starting/running (lock held)")
			return
		}
	}
	if lp := os.Getenv("ABSD_LOG"); lp != "" {
		if f, err := os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			log.SetOutput(f)
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)

	opts := service.Options{
		DataDir: env("ABSD_DATA", defaultDataDir()),
		SDCard:  env("SDCARD_PATH", "/mnt/SDCARD"),
		FFmpeg:  env("ABSD_FFMPEG", defaultFFmpeg()),
		Output:  env("ABSD_AUDIO_OUT", "alsa"),
		// NextUI sets HOME to the userdata dir; ALSA reads $HOME/.asoundrc.
		UserData: env("ABSD_USERDATA", env("USERDATA_PATH", os.Getenv("HOME"))),
	}
	log.Printf("absd %s starting (data=%s ffmpeg=%s out=%s)", service.Version, opts.DataDir, opts.FFmpeg, opts.Output)
	if err := player.Probe(context.Background(), opts.FFmpeg); err != nil {
		log.Printf("warning: %v", err)
	}
	svc, err := service.New(opts)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	if err := svc.StartProxy(); err != nil {
		log.Fatalf("proxy: %v", err)
	}

	hk := hotkeys.New(svc.HotkeysActive, svc.Hotkey)
	applyHotkeys := func(set store.Settings) {
		if err := hk.SetBindings(set.Hotkeys.Specs()); err != nil {
			log.Printf("hotkeys: %v", err)
		}
	}
	applyHotkeys(svc.St.GetSettings())
	svc.OnSettings(applyHotkeys)
	if os.Getenv("ABSD_NO_HOTKEYS") == "" {
		go hk.Run()
	}

	go svc.Run()
	go func() {
		if err := svc.ServeControl(socketPath()); err != nil {
			log.Fatalf("control socket: %v", err)
		}
	}()
	go func() {
		ctx := context.Background()
		svc.CheckOnline(ctx)
		for {
			select {
			case <-svc.Done():
				return
			case <-time.After(30 * time.Second):
			}
			if !svc.Online() {
				svc.CheckOnline(ctx)
			}
		}
	}()

	idleLimit := 3 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("ABSD_IDLE_EXIT")); err == nil {
		idleLimit = v
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	check := time.NewTicker(15 * time.Second)
	defer check.Stop()
	for {
		select {
		case s := <-sig:
			log.Printf("received %v, saving and exiting", s)
			svc.Shutdown()
			svc.Quit()
		case <-svc.Done():
			_ = os.Remove(socketPath())
			log.Printf("bye")
			return
		case <-check.C:
			if idleLimit > 0 && svc.IdleFor() > idleLimit {
				log.Printf("idle for %v, exiting", idleLimit)
				svc.Quit()
			}
		}
	}
}
