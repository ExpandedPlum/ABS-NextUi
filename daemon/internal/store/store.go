// Package store persists daemon state as small JSON files on the SD card.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
)

// Hotkeys map actions to button specs like "FN1" or "SELECT+R1"; several
// alternatives can be comma separated.
type Hotkeys struct {
	Enabled     bool   `json:"enabled"`
	Toggle      string `json:"toggle"`
	Back        string `json:"back"`
	Forward     string `json:"forward"`
	PrevChapter string `json:"prevChapter"`
	NextChapter string `json:"nextChapter"`
}

func (h Hotkeys) Specs() map[string]string {
	return map[string]string{
		"toggle": h.Toggle, "back": h.Back, "forward": h.Forward,
		"prevChapter": h.PrevChapter, "nextChapter": h.NextChapter,
	}
}

type Settings struct {
	SkipBack      int     `json:"skipBack"`
	SkipForward   int     `json:"skipForward"`
	ResumeRewind  int     `json:"resumeRewind"`
	Speed         float64 `json:"speed"`
	InsecureTLS   bool    `json:"insecureTLS"`
	DownloadsDir  string  `json:"downloadsDir"`
	LibraryID     string  `json:"libraryId"`
	SyncIntervalS int     `json:"syncInterval"`
	Hotkeys       Hotkeys `json:"hotkeys"`
	AudioDevice   string  `json:"audioDevice"`
	PauseOnSleep  bool    `json:"pauseOnSleep"`
	LibrarySort   string  `json:"librarySort"`
}

func DefaultSettings(sdcard string) Settings {
	return Settings{
		SkipBack:      15,
		SkipForward:   30,
		ResumeRewind:  2,
		Speed:         1,
		DownloadsDir:  filepath.Join(sdcard, "Audiobooks"),
		SyncIntervalS: 15,
		Hotkeys: Hotkeys{
			Enabled:     true,
			Toggle:      "FN1,SELECT+START",
			Back:        "SELECT+L1",
			Forward:     "SELECT+R1",
			PrevChapter: "",
			NextChapter: "",
		},
		AudioDevice:  "default",
		PauseOnSleep: true,
		LibrarySort:  "title",
	}
}

// LocalProgress is the device's own record of where each book is, so resume
// works offline. Dirty means it has not been confirmed by the server yet.
type LocalProgress struct {
	CurrentTime float64 `json:"currentTime"`
	Duration    float64 `json:"duration"`
	UpdatedAt   int64   `json:"updatedAt"`
	Dirty       bool    `json:"dirty"`
	Finished    bool    `json:"finished"`
}

type Store struct {
	dir string
	mu  sync.Mutex

	Settings Settings
	Creds    abs.Credentials
	DeviceID string
	Progress map[string]LocalProgress
	Pending  map[string]abs.LocalSession // session id -> unsynced listening
}

func Open(dir, sdcard string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:      dir,
		Settings: DefaultSettings(sdcard),
		Progress: map[string]LocalProgress{},
		Pending:  map[string]abs.LocalSession{},
	}
	_ = readJSON(s.path("settings.json"), &s.Settings)
	_ = readJSON(s.path("credentials.json"), &s.Creds)
	_ = readJSON(s.path("progress.json"), &s.Progress)
	_ = readJSON(s.path("pending-sessions.json"), &s.Pending)
	var dev struct {
		ID string `json:"deviceId"`
	}
	_ = readJSON(s.path("device.json"), &dev)
	if dev.ID == "" {
		dev.ID = NewID()
		_ = writeJSON(s.path("device.json"), dev)
	}
	s.DeviceID = dev.ID
	if s.Settings.Speed <= 0 {
		s.Settings.Speed = 1
	}
	if s.Settings.SyncIntervalS < 5 {
		s.Settings.SyncIntervalS = 15
	}
	return s, nil
}

func (s *Store) Dir() string             { return s.dir }
func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }
func (s *Store) CacheDir() string        { return filepath.Join(s.dir, "cache") }

func (s *Store) SaveSettings(set Settings) error {
	s.mu.Lock()
	s.Settings = set
	s.mu.Unlock()
	return writeJSON(s.path("settings.json"), set)
}

func (s *Store) GetSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings
}

func (s *Store) SaveCreds(c abs.Credentials) error {
	s.mu.Lock()
	s.Creds = c
	s.mu.Unlock()
	return writeJSON(s.path("credentials.json"), c)
}

func (s *Store) GetProgress(id string) (LocalProgress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.Progress[id]
	return p, ok
}

func (s *Store) SetProgress(id string, p LocalProgress) {
	s.mu.Lock()
	s.Progress[id] = p
	cp := make(map[string]LocalProgress, len(s.Progress))
	for k, v := range s.Progress {
		cp[k] = v
	}
	s.mu.Unlock()
	_ = writeJSON(s.path("progress.json"), cp)
}

func (s *Store) PutPending(ls abs.LocalSession) {
	s.mu.Lock()
	s.Pending[ls.ID] = ls
	cp := s.pendingCopyLocked()
	s.mu.Unlock()
	_ = writeJSON(s.path("pending-sessions.json"), cp)
}

func (s *Store) DropPending(id string) {
	s.mu.Lock()
	if _, ok := s.Pending[id]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.Pending, id)
	cp := s.pendingCopyLocked()
	s.mu.Unlock()
	_ = writeJSON(s.path("pending-sessions.json"), cp)
}

func (s *Store) PendingSessions() []abs.LocalSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]abs.LocalSession, 0, len(s.Pending))
	for _, v := range s.Pending {
		out = append(out, v)
	}
	return out
}

func (s *Store) pendingCopyLocked() map[string]abs.LocalSession {
	cp := make(map[string]abs.LocalSession, len(s.Pending))
	for k, v := range s.Pending {
		cp[k] = v
	}
	return cp
}

func NowMs() int64 { return time.Now().UnixMilli() }

func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSON writes atomically so a power loss can't leave a truncated file.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteJSON and ReadJSON are exported for other packages (download manifests).
func WriteJSON(path string, v any) error { return writeJSON(path, v) }
func ReadJSON(path string, v any) error  { return readJSON(path, v) }
