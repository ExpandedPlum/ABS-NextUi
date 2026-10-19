# Audiobookshelf for NextUI

An [Audiobookshelf](https://www.audiobookshelf.org/) client pak for
[NextUI](https://nextui.loveretro.games/) on the TrimUI Brick (and Smart Pro,
which shares the `tg5040` platform). It's inspired by
[AudioBooth](https://github.com/AudioBooth/AudioBooth): browse your library,
stream or download books, and listen with the **screen off** or **while you
play a game**.

<p>
<img src="docs/screenshots/home.png" width="49%"> <img src="docs/screenshots/now-playing.png" width="49%">
<img src="docs/screenshots/library.png" width="49%"> <img src="docs/screenshots/book.png" width="49%">
</p>

> **Status: v0.1.0, not yet tested on real hardware.** Everything was built
> with the official NextUI toolchain. The playback service and the device
> ffmpeg were tested end to end against a real Audiobookshelf server, both
> natively and under arm64 emulation. The UI was checked visually at the
> Brick's 1024×768 resolution. Still unverified on a device: backlight
> control, button codes, and whether audio mixes with game audio. See
> [Testing on the device](#testing-on-the-device).

## Features

- Log in with username and password. Works with both new (access/refresh
  token) and legacy Audiobookshelf auth.
- **Continue Listening**, **Library** (sort by title, author, date added or
  length), **Recently Added**, and **Search**.
- Book pages with cover, author, narrator, series, length, progress and
  description.
- Streaming playback of m4b/m4a/mp3/flac/ogg/opus/wma, including books split
  across many files.
- Chapters: L1/R1 to jump, or pick from a list.
- Pitch-preserving **playback speed**, 0.5× to 3×.
- **Sleep timer**: 5 minutes to 2 hours, or end of chapter.
- Configurable skip back/forward, with a small rewind when resuming.
- **Downloads** to the SD card for offline listening, resumable if
  interrupted.
- **Progress sync** with the server every 15 s and on pause. Listening done
  offline is queued and uploaded when the server is reachable again.
- **Screen off**: a short press of POWER turns the backlight off and audio
  keeps playing. The CPU is clocked down and buttons are ignored until you
  press POWER again. Volume buttons still work.
- **Background playback**: exit the app with *Keep listening* and the book
  keeps playing in the NextUI menu and inside games.
- **In-game controls**: play/pause and skip without leaving your game.
- Progress is saved automatically before the device sleeps or shuts down.

## Install

1. Download `Audiobookshelf.pak.zip` from Releases, or build it (see below).
2. Unzip it and copy the `Audiobookshelf.pak` folder to
   `SD:/Tools/tg5040/` on your SD card.
3. On the device, open **Tools → Audiobookshelf**.
4. Enter your server address (e.g. `192.168.1.20:13378` or
   `https://abs.example.com`), username and password, then choose **Log In**.

## Controls

**Everywhere:** POWER (short) turns the screen off/on and keeps playing.
Holding POWER saves progress and shuts down. MENU shows help.

**Now Playing**

| Button | Action |
|--------|--------|
| A | Play / pause |
| LEFT / RIGHT | Skip back / forward (15 s / 30 s by default) |
| L1 / R1 | Previous / next chapter |
| X | Chapter list |
| Y | Sleep timer |
| SELECT | Playback speed |
| START | Stop playback |
| B | Back to browsing (keeps playing) |

**Lists:** A opens, X plays straight away, Y changes sort (Library), B goes
back. On a book page, Y (MORE) offers: play from the beginning or a chapter,
download/remove, and mark finished.

**Leaving the app:** press B on the home screen and choose *Keep listening*
to carry on in the background, or *Stop playback and exit*.

### In-game controls

While a book is loaded and you're outside the app (in a game or the NextUI
menu):

| Buttons | Action |
|---------|--------|
| FN1, or SELECT+START | Play / pause |
| SELECT+L1 | Skip back |
| SELECT+R1 | Skip forward |

The game also receives these presses; that can't be avoided without blocking
input to the game. You can turn in-game controls off in Settings. You can
remap them by editing `hotkeys` in
`.userdata/shared/Audiobookshelf/settings.json`. Buttons: `A B X Y L1 R1 L2
R2 SELECT START MENU FN1 FN2`. Use `+` for chords and `,` for alternatives,
e.g. `"toggle": "FN2,SELECT+Y"`.

## How it works

```
 ┌───────────────┐   unix socket    ┌──────────────────────────────┐
 │ abs-ui (C)    │  /tmp/absd.sock  │ absd (Go, background daemon) │
 │ Apostrophe UI │ ───────────────▶ │ Audiobookshelf API + auth    │
 │ screen-off    │   JSON commands  │ progress sync / offline queue│
 └───────────────┘                  │ downloads, sleep timer       │
                                    │ in-game hotkeys (evdev)      │
                                    │ loopback HTTP → server proxy │
                                    └──────────────┬───────────────┘
                                                   │ spawns
                                         ┌─────────▼─────────┐
                                         │ ffmpeg (bundled)  │ decode + atempo
                                         │ → ALSA "default"  │ → speaker / BT
                                         └───────────────────┘
```

- `launch.sh` starts **absd** detached from the launcher, then runs the UI.
  When you leave the UI, absd keeps running. It exits on its own after a
  few idle minutes once nothing is loaded.
- **absd** plays one audio file at a time through a small bundled ffmpeg
  (1.9 MB, audio only), which writes straight to ALSA. Pausing stops ffmpeg,
  which frees the audio device for games. Remote files are fetched through a
  loopback proxy that adds authentication and handles HTTPS, so ffmpeg needs
  no TLS support.
- The **UI** turns off Apostrophe's suspend-on-power handler and uses its
  own. A short press sets the panel brightness to 0 through `/dev/disp`, the
  same ioctl NextUI uses. It also drops the CPU to its lowest preset and
  filters out all input until the next press.
- A NextUI `pre-sleep.d` hook (`.userdata/tg5040/.hooks/pre-sleep.d/`)
  pauses and syncs before the device suspends.

Files on the SD card:

| Path | Contents |
|------|----------|
| `Tools/tg5040/Audiobookshelf.pak/` | The pak |
| `.userdata/shared/Audiobookshelf/` | Settings, login tokens, local progress, offline sync queue, cover cache |
| `Audiobooks/` | Downloaded books (one folder per book, with `manifest.json`) |
| `.userdata/tg5040/logs/Audiobookshelf*.txt` | UI and daemon logs |

## Testing on the device

Things that still need confirming on real hardware, in rough priority order:

1. **Basic playback**: log in, play a book, and check you hear audio. If you
   don't, check `logs/Audiobookshelf-daemon.txt`.
2. **Screen off**: a short POWER press should blank the screen with audio
   continuing, and the next press should restore the previous brightness.
   The volume buttons should still work while dark.
3. **Behind a game**: start a book, exit with *Keep listening*, and launch a
   game. If the book stops, or the game has no sound, the daemon log will
   show `Audio device is busy`. That means the stock ALSA config doesn't mix
   streams. Please report it, along with the output of
   `cat /etc/asound.conf` and `aplay -L`.
4. **In-game buttons**: check that FN1 toggles playback in a game. The button
   codes were derived from NextUI's tg5040 input map, not read from hardware.
5. **Sleep**: put the device to sleep from the NextUI menu; it should pause,
   and after waking the progress should be on the server.

## Building

Requirements: Go 1.26+ and Docker. The NextUI toolchain image
`ghcr.io/loveretro/tg5040-toolchain` is pulled automatically.

```sh
make            # → dist/Audiobookshelf.pak.zip
make daemon     # build/tg5040/absd    (static arm64 Go binary)
make ui         # build/tg5040/abs-ui  (C, built in the toolchain container)
make ffmpeg     # build/tg5040/ffmpeg  (see scripts/build-ffmpeg.sh)
```

### Desktop development

```sh
sudo apt install libsdl2-dev libsdl2-ttf-dev libsdl2-image-dev ffmpeg
make desktop
ABSD_AUDIO_OUT=null ABSD_FFMPEG=$(which ffmpeg) build/desktop/absd serve &
cd somewhere-with-a-font.ttf && AP_WINDOW_WIDTH=1024 AP_WINDOW_HEIGHT=768 /path/to/build/desktop/abs-ui
```

Keyboard on desktop: arrows, `a` `b` `x` `y`, `l`/`r` for L1/R1, Enter = START,
Space = SELECT, `h` = MENU. Useful daemon environment variables:
`ABSD_AUDIO_OUT=null` (no sound card), `ABSD_SOCKET`, `ABSD_DATA`,
`ABSD_IDLE_EXIT=0`, `ABSD_NO_HOTKEYS=1`.

You can drive the daemon directly too: `absd ctl status`, `absd ctl toggle`,
`absd ctl play '{"id":"<item id>"}'`, `absd ctl skip '{"seconds":-30}'`.

### Tests

```sh
cd daemon && go test ./...        # unit tests
scripts/integration-test.sh       # real Audiobookshelf server in Docker
```

The integration test streams a chaptered m4b and a multi-file mp3 book. It
checks chapter tracking, track changes, server progress and auto-finish, then
downloads a book, plays it with the server unreachable, and verifies the
queued progress uploads once the server is back.

## Not yet supported

- Podcasts (the libraries are hidden).
- OpenID/SSO login and API-key login.
- Bookmarks, collections, series and author browsing.

## Credits

[AudioBooth](https://github.com/AudioBooth/AudioBooth) for the feature set and
API reference. [Apostrophe](https://github.com/Helaas/Apostrophe) by Kevin
Vranken and [gabagool](https://github.com/BrandonKowalski/gabagool) by Brandon
Kowalski for the NextUI UI toolkits. Jose Diaz-Gonzalez's
[pak guide](https://josediazgonzalez.com/2025/06/16/writing-a-pak-for-the-minui-and-nextui-launchers/).
Third-party licenses are in [`licenses/`](licenses/THIRD_PARTY.md).
