// Package service is the daemon's brain: it owns the player, keeps listening
// progress in sync with the server (or queues it while offline), runs the
// sleep timer and answers the UI.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/downloads"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/gameaudio"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/store"
)

// Version is set at build time (-ldflags -X …service.Version=…).
var Version = "dev"

type current struct {
	itemID        string
	episodeID     string
	media         *player.Media
	serverSession string
	sessionBroken bool
	local         abs.LocalSession
	coverPath     string
	isLocalFiles  bool
	pausedAt      time.Time
	lastSync      time.Time
}

type sleepTimer struct {
	active       bool
	endOfChapter bool
	remaining    time.Duration
	chapterEnd   float64
}

type Service struct {
	St     *store.Store
	Client *abs.Client
	Eng    *player.Engine
	DL     *downloads.Manager

	proxyBase string

	mu       sync.Mutex
	cur      *current
	sleep    sleepTimer
	online   bool
	uiSeen   time.Time
	lastBusy time.Time
	quit     chan struct{}
	syncMu   sync.Mutex

	onSettings func(store.Settings)

	progCache   map[string]abs.MediaProgress
	progCacheAt time.Time
	phone       *phoneServer

	game        *gameaudio.Router
	gameMu      sync.Mutex
	gameErr     string
	gameMutedAt time.Time

	sinkMu      sync.Mutex
	sinkSig     string // audio routing when the book's output was opened
	sinkChecked time.Time

	trans *time.Timer // pending resume after an app switch (TransitionPause)
}

// OnSettings registers a callback run after settings change.
func (s *Service) OnSettings(fn func(store.Settings)) { s.onSettings = fn }

type Options struct {
	DataDir  string
	UserData string // NextUI's per-platform userdata dir (where .asoundrc lives)
	SDCard   string
	FFmpeg   string
	Output   string
}

func New(opts Options) (*Service, error) {
	st, err := store.Open(opts.DataDir, opts.SDCard)
	if err != nil {
		return nil, err
	}
	set := st.GetSettings()
	s := &Service{St: st, quit: make(chan struct{}), lastBusy: time.Now()}
	device := abs.DeviceInfo{
		DeviceID:      st.DeviceID,
		ClientName:    "ABS NextUI",
		ClientVersion: Version,
		Manufacturer:  "TrimUI",
		Model:         deviceModel(),
	}
	s.Client = abs.NewClient(device, set.InsecureTLS, func(c abs.Credentials) {
		if err := st.SaveCreds(c); err != nil {
			log.Printf("saving credentials: %v", err)
		}
	})
	s.Client.SetCredentials(st.Creds)
	if opts.UserData != "" {
		s.game = gameaudio.New(opts.UserData)
		if conf := os.Getenv("ABSD_ASOUND_CONF"); conf != "" { // tests
			s.game.SystemConf = conf
		}
	}
	s.Eng = player.New(player.Config{FFmpeg: opts.FFmpeg, Output: opts.Output, Device: set.AudioDevice,
		DeviceFunc: s.bookDevice}, s.onPlayerEvent)
	s.Eng.SetSpeed(set.Speed)
	s.DL = downloads.New(s.Client, st)
	if err := os.MkdirAll(filepath.Join(st.CacheDir(), "covers"), 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

func deviceModel() string {
	switch os.Getenv("DEVICE") {
	case "brick":
		return "TrimUI Brick"
	case "brickpro":
		return "TrimUI Brick Pro"
	case "smartpro":
		return "TrimUI Smart Pro"
	}
	if p := os.Getenv("PLATFORM"); p != "" {
		return p
	}
	return "NextUI"
}

// StartProxy serves audio from the server to ffmpeg over plain loopback HTTP,
// adding auth and handling HTTPS/token refresh so ffmpeg doesn't need TLS.
func (s *Service) StartProxy() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.proxyBase = "http://" + ln.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(s.serveProxy)}
	go func() { log.Printf("proxy stopped: %v", srv.Serve(ln)) }()
	return nil
}

func (s *Service) serveProxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.RequestURI(), "/s")
	if !strings.HasPrefix(path, "/api/") {
		http.NotFound(w, r)
		return
	}
	hdr := http.Header{}
	if rg := r.Header.Get("Range"); rg != "" {
		hdr.Set("Range", rg)
	}
	resp, err := s.Client.Open(r.Context(), http.MethodGet, path, hdr, nil, true)
	if err != nil {
		s.noteNetErr(err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	s.setOnline(true)
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Run drives the background loops until Quit.
func (s *Service) Run() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastTick := time.Now()
	lastSync := time.Now()
	lastFlush := time.Time{}
	for {
		select {
		case <-s.quit:
			return
		case now := <-tick.C:
			dt := now.Sub(lastTick)
			lastTick = now
			s.tickSleep(dt)

			snap := s.Eng.Snapshot()
			interval := time.Duration(s.St.GetSettings().SyncIntervalS) * time.Second
			if snap.State == player.StatePlaying && now.Sub(lastSync) >= interval {
				lastSync = now
				go s.Sync(false)
			}
			if now.Sub(lastFlush) > 60*time.Second {
				lastFlush = now
				go s.flushPending()
			}
			state := snap.State
			if s.inTransition() {
				// Keep games muted while the book is held for an app switch,
				// so the new game's first sounds don't leak through.
				state = player.StatePlaying
			}
			s.reconcileGameAudio(state, now)
			s.followSink(snap.State, now)
			if snap.State == player.StatePlaying || snap.State == player.StateLoading || s.DL.Busy() || s.uiActive() {
				s.mu.Lock()
				s.lastBusy = now
				s.mu.Unlock()
			}
		}
	}
}

func (s *Service) Quit() {
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
}

func (s *Service) Done() <-chan struct{} { return s.quit }

// IdleFor reports how long nothing has needed the daemon (no playback, no
// downloads, no UI). A paused book keeps the daemon alive so in-game
// shortcuts can resume it.
func (s *Service) IdleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		return 0
	}
	return time.Since(s.lastBusy)
}

func (s *Service) UIHeartbeat(foreground bool) {
	s.mu.Lock()
	if foreground {
		s.uiSeen = time.Now()
	} else {
		s.uiSeen = time.Time{}
	}
	s.mu.Unlock()
}

func (s *Service) uiActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.uiSeen.IsZero() && time.Since(s.uiSeen) < 5*time.Second
}

// HotkeysActive is true when a book is loaded and the UI isn't in front (the
// UI has its own controls).
func (s *Service) HotkeysActive() bool {
	if !s.St.GetSettings().Hotkeys.Enabled || s.uiActive() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil
}

func (s *Service) Hotkey(action string) {
	set := s.St.GetSettings()
	var err error
	switch action {
	case "toggle":
		err = s.Toggle()
	case "back":
		err = s.Skip(-float64(set.SkipBack))
	case "forward":
		err = s.Skip(float64(set.SkipForward))
	case "prevChapter":
		err = s.PrevChapter()
	case "nextChapter":
		err = s.NextChapter()
	}
	if err != nil {
		log.Printf("hotkey %s: %v", action, err)
	}
}

func (s *Service) setOnline(v bool) {
	s.mu.Lock()
	changed := s.online != v
	s.online = v
	s.mu.Unlock()
	if changed {
		log.Printf("server reachable: %v", v)
		if v {
			go s.flushPending()
		}
	}
}

func (s *Service) Online() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.online
}

// noteNetErr marks the server unreachable for transport errors. HTTP status
// errors mean the server answered, so it is online.
func (s *Service) noteNetErr(err error) {
	if err == nil {
		s.setOnline(true)
		return
	}
	var he *abs.HTTPError
	if errors.As(err, &he) || errors.Is(err, abs.ErrNotFound) || errors.Is(err, abs.ErrAuth) || errors.Is(err, context.Canceled) {
		s.setOnline(true)
		return
	}
	if errors.Is(err, abs.ErrNotLoggedIn) {
		return
	}
	s.setOnline(false)
}

// netCtx shortens server calls while the server is known to be unreachable,
// so offline browsing falls back to cached data quickly.
func (s *Service) netCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.Online() {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, 4*time.Second)
}

// CheckOnline pings the server to refresh the online flag.
func (s *Service) CheckOnline(ctx context.Context) bool {
	if !s.Client.LoggedIn() {
		s.setOnline(false)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	err := s.Client.Ping(ctx)
	s.noteNetErr(err)
	return s.Online()
}

// ---------------------------------------------------------------------------
// Playback

// key identifies the loaded book or episode in local progress and downloads.
func (c *current) key() string { return downloads.Key(c.itemID, c.episodeID) }

// Play starts a book, or a podcast episode when episodeID is set.
func (s *Service) Play(ctx context.Context, itemID, episodeID string, from *float64) error {
	s.endTransition()
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()

	// Reuse the loaded book unless it is streaming and a download now exists.
	if cur != nil && cur.itemID == itemID && cur.episodeID == episodeID &&
		(cur.isLocalFiles || s.DL.Get(itemID, episodeID) == nil) {
		if from != nil {
			if err := s.Eng.Seek(*from); err != nil {
				return err
			}
		} else {
			s.applyResumeRewind(cur)
		}
		return s.Eng.Play()
	}
	if cur != nil {
		s.Stop()
	}

	man := s.DL.Get(itemID, episodeID)
	var sess *abs.PlaySession
	// A downloaded book shouldn't wait on a server that's known to be down.
	if s.Client.LoggedIn() && (man == nil || s.Online()) {
		timeout := 12 * time.Second
		if man != nil {
			timeout = 4 * time.Second
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		var err error
		sess, err = s.Client.StartPlay(pctx, itemID, episodeID)
		cancel()
		s.noteNetErr(err)
		if err != nil {
			log.Printf("start session for %s: %v", itemID, err)
			sess = nil
			if man == nil {
				return fmt.Errorf("could not start playback: %w", err)
			}
		}
	}

	var media *player.Media
	switch {
	case man != nil:
		media = man.Media()
	case sess != nil:
		media = s.mediaFromSession(sess)
	default:
		return errors.New("server unreachable and book isn't downloaded")
	}
	if len(media.Tracks) == 0 {
		return errors.New("book has no audio tracks")
	}
	if sess != nil {
		if media.Title == "" {
			media.Title = sess.DisplayTitle
		}
		if media.Author == "" {
			media.Author = sess.DisplayAuthor
		}
		if len(media.Chapters) == 0 {
			for _, c := range sess.Chapters {
				media.Chapters = append(media.Chapters, player.Chapter{Start: c.Start, End: c.End, Title: c.Title})
			}
		}
		if media.Duration <= 0 {
			media.Duration = sess.Duration
		}
	}

	pos := 0.0
	lp, haveLocal := s.St.GetProgress(downloads.Key(itemID, episodeID))
	switch {
	case from != nil:
		pos = *from
	case sess != nil && !(haveLocal && lp.Dirty):
		pos = sess.CurrentTime
	case haveLocal:
		pos = lp.CurrentTime
	}
	if media.Duration > 0 && pos >= media.Duration-2 {
		pos = 0 // finished books start over
	}

	now := store.NowMs()
	mediaType := "book"
	if episodeID != "" {
		mediaType = "podcast"
	}
	media.EpisodeID = episodeID
	nc := &current{
		itemID:       itemID,
		episodeID:    episodeID,
		media:        media,
		isLocalFiles: man != nil,
		local: abs.LocalSession{
			ID: store.NewID(), LibraryItemID: itemID, EpisodeID: episodeID, MediaType: mediaType,
			DisplayTitle: media.Title, DisplayAuthor: media.Author,
			Duration: media.Duration, PlayMethod: 3, MediaPlayer: "ffmpeg",
			DeviceInfo: s.Client.Device, StartTime: pos, CurrentTime: pos,
			StartedAt: now, UpdatedAt: now,
		},
	}
	if sess != nil {
		nc.serverSession = sess.ID
		nc.local.ID = sess.ID
	}
	if man != nil {
		nc.coverPath = man.CoverPath()
	} else {
		nc.coverPath = s.coverFile(itemID)
		go s.EnsureCover(context.Background(), itemID)
	}

	s.mu.Lock()
	s.cur = nc
	s.sleep = sleepTimer{}
	s.mu.Unlock()

	s.Eng.Load(media, pos)
	return s.Eng.Play()
}

func (s *Service) mediaFromSession(sess *abs.PlaySession) *player.Media {
	m := &player.Media{ItemID: sess.LibraryItemID, Title: sess.DisplayTitle, Author: sess.DisplayAuthor, Duration: sess.Duration}
	for _, t := range sess.AudioTracks {
		src := t.ContentURL
		if strings.HasPrefix(src, "/") {
			src = s.proxyBase + "/s" + src
		}
		m.Tracks = append(m.Tracks, player.Track{StartOffset: t.StartOffset, Duration: t.Duration, Source: src})
	}
	if m.Duration <= 0 && len(m.Tracks) > 0 {
		last := m.Tracks[len(m.Tracks)-1]
		m.Duration = last.StartOffset + last.Duration
	}
	return m
}

func (s *Service) applyResumeRewind(cur *current) {
	rw := s.St.GetSettings().ResumeRewind
	snap := s.Eng.Snapshot()
	if rw <= 0 || snap.State != player.StatePaused || cur.pausedAt.IsZero() || time.Since(cur.pausedAt) < 3*time.Second {
		return
	}
	_ = s.Eng.Seek(snap.Position - float64(rw))
}

func (s *Service) Pause() {
	s.endTransition()
	s.Eng.Pause()
	s.mu.Lock()
	if s.cur != nil {
		s.cur.pausedAt = time.Now()
	}
	s.mu.Unlock()
	go s.Sync(false)
}

func (s *Service) Resume() error {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		return errors.New("nothing is playing")
	}
	if !s.endTransition() {
		s.applyResumeRewind(cur)
	}
	return s.Eng.Play()
}

func (s *Service) Toggle() error {
	if s.inTransition() {
		s.Pause()
		return nil
	}
	switch s.Eng.Snapshot().State {
	case player.StatePlaying, player.StateLoading:
		s.Pause()
		return nil
	}
	return s.Resume()
}

func (s *Service) Seek(pos float64) error {
	err := s.Eng.Seek(pos)
	go s.Sync(false)
	return err
}

func (s *Service) Skip(delta float64) error {
	return s.Seek(s.Eng.Position() + delta)
}

func (s *Service) chapterAt(pos float64) (int, *player.Chapter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return -1, nil
	}
	ch := s.cur.media.Chapters
	for i := range ch {
		if pos >= ch[i].Start && (pos < ch[i].End || i == len(ch)-1) {
			return i, &ch[i]
		}
	}
	return -1, nil
}

func (s *Service) SeekChapter(i int) error {
	s.mu.Lock()
	if s.cur == nil || i < 0 || i >= len(s.cur.media.Chapters) {
		s.mu.Unlock()
		return errors.New("no such chapter")
	}
	start := s.cur.media.Chapters[i].Start
	s.mu.Unlock()
	return s.Seek(start)
}

func (s *Service) NextChapter() error {
	i, _ := s.chapterAt(s.Eng.Position())
	return s.SeekChapter(i + 1)
}

func (s *Service) PrevChapter() error {
	pos := s.Eng.Position()
	i, ch := s.chapterAt(pos)
	if ch == nil {
		return s.Seek(0)
	}
	if pos-ch.Start > 3 || i == 0 {
		return s.Seek(ch.Start)
	}
	return s.SeekChapter(i - 1)
}

func (s *Service) SetSpeed(v float64) error {
	set := s.St.GetSettings()
	set.Speed = v
	_ = s.St.SaveSettings(set)
	return s.Eng.SetSpeed(v)
}

// SetSleep sets a timer in minutes; minutes < 0 means "end of chapter",
// 0 cancels.
func (s *Service) SetSleep(minutes float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case minutes == 0:
		s.sleep = sleepTimer{}
	case minutes < 0:
		s.sleep = sleepTimer{active: true, endOfChapter: true}
	default:
		s.sleep = sleepTimer{active: true, remaining: time.Duration(minutes * float64(time.Minute))}
	}
}

func (s *Service) tickSleep(dt time.Duration) {
	snap := s.Eng.Snapshot()
	if snap.State != player.StatePlaying {
		return
	}
	s.mu.Lock()
	sl := s.sleep
	if !sl.active {
		s.mu.Unlock()
		return
	}
	fire := false
	if sl.endOfChapter {
		s.mu.Unlock()
		_, ch := s.chapterAt(snap.Position)
		s.mu.Lock()
		if ch != nil {
			if s.sleep.chapterEnd == 0 {
				s.sleep.chapterEnd = ch.End
			}
			fire = snap.Position >= s.sleep.chapterEnd-0.25
		}
	} else {
		s.sleep.remaining -= dt
		fire = s.sleep.remaining <= 0
	}
	if fire {
		s.sleep = sleepTimer{}
	}
	s.mu.Unlock()
	if fire {
		log.Printf("sleep timer: pausing")
		s.Pause()
	}
}

// Stop syncs, closes the server session and unloads the book.
func (s *Service) Stop() {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	s.endTransition()
	if cur == nil {
		return
	}
	s.Eng.Pause()
	s.Sync(true)
	s.Eng.Unload()
	s.mu.Lock()
	if s.cur == cur {
		s.cur = nil
		s.sleep = sleepTimer{}
	}
	s.lastBusy = time.Now()
	s.mu.Unlock()
}

func (s *Service) onPlayerEvent(ev player.Event) {
	switch ev {
	case player.EventEnded:
		s.mu.Lock()
		cur := s.cur
		s.mu.Unlock()
		if cur == nil {
			return
		}
		s.Sync(false)
		s.markFinished(cur.itemID, cur.episodeID, true)
	case player.EventError:
		go s.Sync(false)
	}
}

// Sync records the current position locally and reports it to the server.
// final also closes the server session.
func (s *Service) Sync(final bool) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		return
	}
	pos := s.Eng.Position()
	listened := s.Eng.TakeListened()
	now := store.NowMs()

	s.mu.Lock()
	cur.local.CurrentTime = pos
	cur.local.TimeListening += listened
	cur.local.UpdatedAt = now
	ls := cur.local
	sessID, broken := cur.serverSession, cur.sessionBroken
	cur.lastSync = time.Now()
	s.mu.Unlock()

	s.St.SetProgress(cur.key(), store.LocalProgress{CurrentTime: pos, Duration: cur.media.Duration, UpdatedAt: now, Dirty: true})
	if !s.Client.LoggedIn() {
		s.St.PutPending(ls)
		return
	}

	timeout := 10 * time.Second
	if !s.Online() {
		timeout = 3 * time.Second // likely to fail; queue it without holding things up
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var err error
	if sessID != "" && !broken {
		err = s.Client.SyncSession(ctx, sessID, pos, listened, cur.media.Duration)
		if err == nil && final {
			err = s.Client.CloseSession(ctx, sessID, pos, 0, cur.media.Duration)
		}
		if err != nil {
			log.Printf("sync session: %v", err)
			s.mu.Lock()
			cur.sessionBroken = true
			s.mu.Unlock()
		}
	}
	if sessID == "" || broken || err != nil {
		err = s.Client.SyncLocalSession(ctx, ls)
	}
	s.noteNetErr(err)
	if err != nil {
		log.Printf("sync (queued for later): %v", err)
		s.St.PutPending(ls)
		return
	}
	s.St.DropPending(ls.ID)
	s.St.SetProgress(cur.key(), store.LocalProgress{CurrentTime: pos, Duration: cur.media.Duration, UpdatedAt: now})
	s.invalidateProgress()
}

// flushPending uploads listening that happened while offline.
func (s *Service) flushPending() {
	if s.Client.LoggedIn() {
		s.flushBookmarks()
	}
	pend := s.St.PendingSessions()
	if len(pend) == 0 || !s.Client.LoggedIn() {
		return
	}
	s.mu.Lock()
	activeID := ""
	if s.cur != nil {
		activeID = s.cur.local.ID
	}
	s.mu.Unlock()
	for _, ls := range pend {
		if ls.ID == activeID {
			continue // the regular sync loop owns it
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.Client.SyncLocalSession(ctx, ls)
		cancel()
		s.noteNetErr(err)
		if err != nil {
			log.Printf("flush pending %s: %v", ls.ID, err)
			return
		}
		s.St.DropPending(ls.ID)
		key := downloads.Key(ls.LibraryItemID, ls.EpisodeID)
		if lp, ok := s.St.GetProgress(key); ok && lp.UpdatedAt <= ls.UpdatedAt {
			lp.Dirty = false
			s.St.SetProgress(key, lp)
		}
		log.Printf("uploaded offline session %s", ls.ID)
	}
}

func (s *Service) markFinished(itemID, episodeID string, finished bool) error {
	key := downloads.Key(itemID, episodeID)
	lp, _ := s.St.GetProgress(key)
	lp.Finished = finished
	lp.UpdatedAt = store.NowMs()
	if !finished {
		lp.CurrentTime = 0
	}
	s.St.SetProgress(key, lp)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	patch := map[string]any{"isFinished": finished}
	if !finished {
		patch["currentTime"] = 0
		patch["progress"] = 0
	}
	err := s.Client.UpdateProgress(ctx, itemID, episodeID, patch)
	s.noteNetErr(err)
	s.invalidateProgress()
	return err
}

func (s *Service) MarkFinished(itemID, episodeID string, finished bool) error {
	s.mu.Lock()
	playing := s.cur != nil && s.cur.itemID == itemID && s.cur.episodeID == episodeID
	s.mu.Unlock()
	if playing && finished {
		s.Stop()
	}
	return s.markFinished(itemID, episodeID, finished)
}

// PreSleep runs from NextUI's pre-sleep hook: pause and push progress before
// the device suspends.
func (s *Service) PreSleep() {
	if !s.St.GetSettings().PauseOnSleep {
		return
	}
	held := s.endTransition()
	switch st := s.Eng.Snapshot().State; {
	case held, st == player.StatePlaying, st == player.StateLoading:
		s.Eng.Pause()
		s.mu.Lock()
		if s.cur != nil {
			s.cur.pausedAt = time.Now()
		}
		s.mu.Unlock()
		// Don't hold up the suspend for long; a sync interrupted by sleep is
		// queued and retried after wake.
		done := make(chan struct{})
		go func() { s.Sync(false); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

// Shutdown is called on SIGTERM or quit: save everything.
func (s *Service) Shutdown() {
	s.Stop()
	s.restoreGameAudio()
}

// ---------------------------------------------------------------------------
// Covers

func (s *Service) coverFile(id string) string {
	return filepath.Join(s.St.CacheDir(), "covers", sanitizeID(id)+".jpg")
}

func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' {
			return '_'
		}
		return r
	}, id)
}

// EnsureCover returns a local cover path, downloading it if needed. Returns
// "" when no cover is available.
func (s *Service) EnsureCover(ctx context.Context, id string) string {
	if p := s.DL.CoverFor(id); p != "" {
		return p
	}
	path := s.coverFile(id)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return path
	}
	if !s.Client.LoggedIn() {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := s.Client.Open(ctx, http.MethodGet, abs.CoverPath(id, 400), nil, nil, false)
	if err != nil {
		s.noteNetErr(err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return ""
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return ""
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		return ""
	}
	return path
}
