package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/store"
)

// Summary is the compact book representation sent to the UI for lists.
type Summary struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Author     string  `json:"author"`
	Series     string  `json:"series,omitempty"`
	Duration   float64 `json:"duration"`
	Progress   float64 `json:"progress"`
	Finished   bool    `json:"finished"`
	Downloaded bool    `json:"downloaded"`
}

type Detail struct {
	Summary
	Subtitle    string           `json:"subtitle,omitempty"`
	Narrator    string           `json:"narrator,omitempty"`
	Description string           `json:"description,omitempty"`
	Year        string           `json:"year,omitempty"`
	Genres      []string         `json:"genres,omitempty"`
	CurrentTime float64          `json:"currentTime"`
	Chapters    []player.Chapter `json:"chapters"`
	Cover       string           `json:"cover,omitempty"`
	Download    any              `json:"download,omitempty"`
	NumTracks   int              `json:"numTracks"`
}

// progressMap returns the user's server-side progress keyed by item id. List
// endpoints don't include it, so it comes from /api/me (cached briefly).
func (s *Service) progressMap(ctx context.Context) map[string]abs.MediaProgress {
	s.mu.Lock()
	if s.progCache != nil && time.Since(s.progCacheAt) < 20*time.Second {
		m := s.progCache
		s.mu.Unlock()
		return m
	}
	s.mu.Unlock()
	if !s.Online() {
		return nil
	}
	me, err := s.Client.Me(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]abs.MediaProgress, len(me.MediaProgress))
	for _, p := range me.MediaProgress {
		m[p.LibraryItemID] = p
	}
	s.mu.Lock()
	s.progCache, s.progCacheAt = m, time.Now()
	s.mu.Unlock()
	return m
}

func (s *Service) invalidateProgress() {
	s.mu.Lock()
	s.progCache = nil
	s.mu.Unlock()
}

func (s *Service) summarize(it abs.LibraryItem) Summary {
	md := it.Media.Metadata
	sm := Summary{ID: it.ID, Title: md.Title, Author: md.AuthorName, Series: md.SeriesName, Duration: it.Media.Duration}
	if sm.Author == "" && len(md.Authors) > 0 {
		names := make([]string, 0, len(md.Authors))
		for _, a := range md.Authors {
			names = append(names, a.Name)
		}
		sm.Author = strings.Join(names, ", ")
	}
	if p := it.UserMediaProgress; p != nil {
		sm.Progress, sm.Finished = p.Progress, p.IsFinished
	}
	// Unsynced local listening is newer than what the server knows.
	if lp, ok := s.St.GetProgress(it.ID); ok && lp.Dirty && lp.Duration > 0 {
		sm.Progress = lp.CurrentTime / lp.Duration
	}
	sm.Downloaded = s.DL.Get(it.ID) != nil
	return sm
}

func (s *Service) summarizeAll(ctx context.Context, items []abs.LibraryItem) []Summary {
	prog := s.progressMap(ctx)
	out := make([]Summary, 0, len(items))
	for _, it := range items {
		if it.MediaType != "" && it.MediaType != "book" {
			continue
		}
		if it.UserMediaProgress == nil {
			if p, ok := prog[it.ID]; ok {
				it.UserMediaProgress = &p
			}
		}
		out = append(out, s.summarize(it))
	}
	return out
}

func (s *Service) Libraries(ctx context.Context) ([]abs.Library, error) {
	libs, err := s.Client.Libraries(ctx)
	s.noteNetErr(err)
	if err != nil {
		var cached []abs.Library
		if store.ReadJSON(s.cachePath("libraries.json"), &cached) == nil {
			return cached, nil
		}
		return nil, err
	}
	books := libs[:0]
	for _, l := range libs {
		if l.MediaType == "" || l.MediaType == "book" {
			books = append(books, l)
		}
	}
	_ = store.WriteJSON(s.cachePath("libraries.json"), books)
	return books, nil
}

func (s *Service) cachePath(name string) string { return filepath.Join(s.St.CacheDir(), name) }

func (s *Service) ContinueListening(ctx context.Context) ([]Summary, error) {
	items, err := s.Client.ItemsInProgress(ctx)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	return s.summarizeAll(ctx, items), nil
}

var sortKeys = map[string]struct {
	key  string
	desc bool
}{
	"title":  {"media.metadata.title", false},
	"author": {"media.metadata.authorName", false},
	"added":  {"addedAt", true},
	"length": {"media.duration", false},
}

// Library returns every book in a library. Results are cached so the list
// still opens offline.
func (s *Service) Library(ctx context.Context, libraryID, sortBy string) ([]Summary, bool, error) {
	sk, ok := sortKeys[sortBy]
	if !ok {
		sk = sortKeys["title"]
		sortBy = "title"
	}
	cacheFile := s.cachePath("library-" + sanitizeID(libraryID) + "-" + sortBy + ".json")
	var all []abs.LibraryItem
	const pageSize = 250
	for page := 0; ; page++ {
		p, err := s.Client.LibraryItems(ctx, libraryID, page, pageSize, sk.key, sk.desc)
		s.noteNetErr(err)
		if err != nil {
			var cached []abs.LibraryItem
			if store.ReadJSON(cacheFile, &cached) == nil {
				return s.summarizeAll(ctx, cached), true, nil
			}
			return nil, false, err
		}
		all = append(all, p.Results...)
		if len(p.Results) < pageSize || len(all) >= p.Total {
			break
		}
	}
	_ = store.WriteJSON(cacheFile, all)
	return s.summarizeAll(ctx, all), false, nil
}

func (s *Service) Search(ctx context.Context, libraryID, q string) ([]Summary, error) {
	items, err := s.Client.Search(ctx, libraryID, q)
	s.noteNetErr(err)
	if err != nil {
		// Offline: search downloaded books by title/author.
		var out []Summary
		ql := strings.ToLower(q)
		for _, d := range s.Downloaded() {
			if strings.Contains(strings.ToLower(d.Title+" "+d.Author), ql) {
				out = append(out, d)
			}
		}
		if out != nil {
			return out, nil
		}
		return nil, err
	}
	return s.summarizeAll(ctx, items), nil
}

func (s *Service) Downloaded() []Summary {
	var out []Summary
	for _, m := range s.DL.Completed() {
		sm := Summary{ID: m.ItemID, Title: m.Title, Author: m.Author, Series: m.Series, Duration: m.Duration, Downloaded: true}
		if lp, ok := s.St.GetProgress(m.ItemID); ok && m.Duration > 0 {
			sm.Progress = lp.CurrentTime / m.Duration
			sm.Finished = lp.Finished
		}
		out = append(out, sm)
	}
	return out
}

func (s *Service) Detail(ctx context.Context, id string) (*Detail, error) {
	man := s.DL.Get(id)
	it, err := s.Client.Item(ctx, id)
	s.noteNetErr(err)
	var d Detail
	switch {
	case err == nil:
		md := it.Media.Metadata
		d.Summary = s.summarize(*it)
		d.Subtitle, d.Description, d.Year, d.Genres = md.Subtitle, md.Description, md.PublishedYear, md.Genres
		d.Narrator = md.NarratorName
		if d.Narrator == "" {
			d.Narrator = strings.Join(md.Narrators, ", ")
		}
		if len(md.Series) > 0 {
			d.Series = md.Series[0].Name
			if md.Series[0].Sequence != "" {
				d.Series += " #" + md.Series[0].Sequence
			}
		}
		for _, c := range it.Media.Chapters {
			d.Chapters = append(d.Chapters, player.Chapter{Start: c.Start, End: c.End, Title: c.Title})
		}
		d.NumTracks = len(it.Media.Tracks)
		if p := it.UserMediaProgress; p != nil {
			d.CurrentTime = p.CurrentTime
		}
	case man != nil:
		d.Summary = Summary{ID: man.ItemID, Title: man.Title, Author: man.Author, Series: man.Series, Duration: man.Duration, Downloaded: true}
		d.Subtitle, d.Narrator, d.Description, d.Year = man.Subtitle, man.Narrator, man.Description, man.Year
		d.Chapters = man.Chapters
		d.NumTracks = len(man.Tracks)
	default:
		return nil, err
	}
	if lp, ok := s.St.GetProgress(id); ok && (lp.Dirty || err != nil) {
		d.CurrentTime = lp.CurrentTime
		if d.Duration > 0 {
			d.Progress = lp.CurrentTime / d.Duration
		}
	}
	if d.Chapters == nil {
		d.Chapters = []player.Chapter{}
	}
	d.Cover = s.EnsureCover(ctx, id)
	if j := s.DL.JobFor(id); j != nil {
		d.Download = j
	}
	return &d, nil
}

func (s *Service) QueueDownload(ctx context.Context, id string) error {
	if !s.Client.LoggedIn() {
		return abs.ErrNotLoggedIn
	}
	title := id
	if it, err := s.Client.Item(ctx, id); err == nil {
		title = it.Media.Metadata.Title
	}
	if err := os.MkdirAll(s.St.GetSettings().DownloadsDir, 0o755); err != nil {
		return err
	}
	s.DL.Enqueue(id, title)
	return nil
}

func (s *Service) DeleteDownload(id string) error {
	s.mu.Lock()
	playing := s.cur != nil && s.cur.itemID == id && s.cur.isLocalFiles
	s.mu.Unlock()
	if playing {
		return errors.New("stop playback before deleting this download")
	}
	return s.DL.Delete(id)
}

// ---------------------------------------------------------------------------
// Status

type PlayerStatus struct {
	State        string  `json:"state"`
	ItemID       string  `json:"itemId,omitempty"`
	Title        string  `json:"title,omitempty"`
	Author       string  `json:"author,omitempty"`
	Cover        string  `json:"cover,omitempty"`
	Position     float64 `json:"position"`
	Duration     float64 `json:"duration"`
	Speed        float64 `json:"speed"`
	ChapterIndex int     `json:"chapterIndex"`
	ChapterCount int     `json:"chapterCount"`
	ChapterTitle string  `json:"chapterTitle,omitempty"`
	ChapterStart float64 `json:"chapterStart"`
	ChapterEnd   float64 `json:"chapterEnd"`
	SleepActive  bool    `json:"sleepActive"`
	SleepChapter bool    `json:"sleepEndOfChapter"`
	SleepLeft    float64 `json:"sleepRemaining"`
	Error        string  `json:"error,omitempty"`
	Offline      bool    `json:"offlineFiles"`
}

type Status struct {
	Version   string         `json:"version"`
	LoggedIn  bool           `json:"loggedIn"`
	Server    string         `json:"server,omitempty"`
	Username  string         `json:"username,omitempty"`
	Online    bool           `json:"online"`
	LibraryID string         `json:"libraryId,omitempty"`
	Player    PlayerStatus   `json:"player"`
	Downloads []any          `json:"downloads"`
	Settings  store.Settings `json:"settings"`
	Pending   int            `json:"pendingSyncs"`
	Time      int64          `json:"time"`
}

func (s *Service) Status() Status {
	cr := s.Client.Credentials()
	set := s.St.GetSettings()
	st := Status{
		Version: Version, LoggedIn: s.Client.LoggedIn(), Server: cr.Server, Username: cr.Username,
		Online: s.Online(), LibraryID: set.LibraryID, Settings: set,
		Pending: len(s.St.PendingSessions()), Time: time.Now().Unix(), Downloads: []any{},
	}
	for _, j := range s.DL.Jobs() {
		st.Downloads = append(st.Downloads, j)
	}
	snap := s.Eng.Snapshot()
	ps := PlayerStatus{State: string(snap.State), Speed: snap.Speed, Error: snap.Error, ChapterIndex: -1}
	s.mu.Lock()
	cur := s.cur
	sl := s.sleep
	s.mu.Unlock()
	if cur != nil && snap.Media != nil {
		ps.ItemID, ps.Title, ps.Author = cur.itemID, cur.media.Title, cur.media.Author
		ps.Position, ps.Duration = snap.Position, cur.media.Duration
		ps.Offline = cur.isLocalFiles
		if _, err := os.Stat(cur.coverPath); err == nil {
			ps.Cover = cur.coverPath
		}
		ps.ChapterCount = len(cur.media.Chapters)
		if i, ch := s.chapterAt(snap.Position); ch != nil {
			ps.ChapterIndex, ps.ChapterTitle, ps.ChapterStart, ps.ChapterEnd = i, ch.Title, ch.Start, ch.End
		}
		ps.SleepActive, ps.SleepChapter = sl.active, sl.endOfChapter
		ps.SleepLeft = sl.remaining.Seconds()
	}
	st.Player = ps
	return st
}

// Chapters of the loaded book, for the chapter picker.
func (s *Service) Chapters() []player.Chapter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return []player.Chapter{}
	}
	out := append([]player.Chapter(nil), s.cur.media.Chapters...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
