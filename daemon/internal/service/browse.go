package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/downloads"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/store"
)

// Entry is one row in any list the UI shows. Kind decides what opening it
// does: "book" and "episode" are playable; "podcast", "series", "author",
// "collection" and "playlist" open another list.
type Entry struct {
	Kind       string  `json:"kind"`
	ID         string  `json:"id"`
	EpisodeID  string  `json:"episodeId,omitempty"`
	Title      string  `json:"title"`
	Subtitle   string  `json:"subtitle,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
	Progress   float64 `json:"progress"`
	Finished   bool    `json:"finished"`
	Downloaded bool    `json:"downloaded"`
	Count      int     `json:"count,omitempty"`
	Library    string  `json:"library,omitempty"`
}

type Detail struct {
	Entry
	Author      string           `json:"author,omitempty"`
	Narrator    string           `json:"narrator,omitempty"`
	Series      string           `json:"series,omitempty"`
	Description string           `json:"description,omitempty"`
	Year        string           `json:"year,omitempty"`
	Published   string           `json:"published,omitempty"`
	CurrentTime float64          `json:"currentTime"`
	Chapters    []player.Chapter `json:"chapters"`
	Cover       string           `json:"cover,omitempty"`
	Download    any              `json:"download,omitempty"`
	NumTracks   int              `json:"numTracks"`
}

type PodcastDetail struct {
	Entry
	Author      string  `json:"author,omitempty"`
	Description string  `json:"description,omitempty"`
	Cover       string  `json:"cover,omitempty"`
	Episodes    []Entry `json:"items"`
	Cached      bool    `json:"cached"`
}

// ---------------------------------------------------------------------------
// Progress

// progressMap returns the user's server-side progress keyed by
// downloads.Key(item, episode). List endpoints don't include it, so it comes
// from /api/me (cached briefly).
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
	s.cacheMe(me)
	m := make(map[string]abs.MediaProgress, len(me.MediaProgress))
	for _, p := range me.MediaProgress {
		m[downloads.Key(p.LibraryItemID, p.EpisodeID)] = p
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

// applyProgress fills progress fields from the server map, then lets newer
// unsynced local listening win.
func (s *Service) applyProgress(e *Entry, prog map[string]abs.MediaProgress) {
	key := downloads.Key(e.ID, e.EpisodeID)
	if p, ok := prog[key]; ok {
		e.Progress, e.Finished = p.Progress, p.IsFinished
	}
	if lp, ok := s.St.GetProgress(key); ok && lp.Dirty && lp.Duration > 0 {
		e.Progress = lp.CurrentTime / lp.Duration
	}
	e.Downloaded = s.DL.Get(e.ID, e.EpisodeID) != nil
}

// ---------------------------------------------------------------------------
// Converting server objects to entries

func bookEntry(it abs.LibraryItem) Entry {
	md := it.Media.Metadata
	e := Entry{Kind: "book", ID: it.ID, Title: md.Title, Subtitle: it.Author(), Duration: it.Media.Duration, Library: it.LibraryID}
	if p := it.UserMediaProgress; p != nil {
		e.Progress, e.Finished = p.Progress, p.IsFinished
	}
	return e
}

func podcastEntry(it abs.LibraryItem) Entry {
	md := it.Media.Metadata
	n := it.Media.NumEpisodes
	if n == 0 {
		n = len(it.Media.Episodes)
	}
	return Entry{Kind: "podcast", ID: it.ID, Title: md.Title, Subtitle: it.Author(), Count: n, Library: it.LibraryID}
}

func episodeEntry(itemID, podcastTitle string, ep abs.Episode) Entry {
	sub := podcastTitle
	if d := episodeDate(ep); d != "" {
		if sub != "" {
			sub += " - " + d
		} else {
			sub = d
		}
	}
	return Entry{Kind: "episode", ID: itemID, EpisodeID: ep.ID, Title: ep.Title, Subtitle: sub, Duration: ep.Length()}
}

func episodeDate(ep abs.Episode) string {
	if ep.PublishedAt > 0 {
		return time.UnixMilli(ep.PublishedAt).UTC().Format("2006-01-02")
	}
	return ep.PubDate
}

// itemEntry turns a library item into a book, podcast, or (when the item
// carries a recentEpisode) episode entry.
func itemEntry(it abs.LibraryItem) Entry {
	if it.MediaType == "podcast" {
		if it.RecentEpisode != nil {
			return episodeEntry(it.ID, it.Media.Metadata.Title, *it.RecentEpisode)
		}
		return podcastEntry(it)
	}
	return bookEntry(it)
}

func (s *Service) entries(ctx context.Context, items []abs.LibraryItem) []Entry {
	out := make([]Entry, 0, len(items))
	for _, it := range items {
		out = append(out, itemEntry(it))
	}
	return s.finish(ctx, out)
}

// finish applies progress and download state to playable entries.
func (s *Service) finish(ctx context.Context, es []Entry) []Entry {
	prog := s.progressMap(ctx)
	for i := range es {
		if es[i].Kind == "book" || es[i].Kind == "episode" {
			s.applyProgress(&es[i], prog)
		}
	}
	if es == nil {
		es = []Entry{}
	}
	return es
}

// ---------------------------------------------------------------------------
// Libraries and lists

func (s *Service) cachePath(name string) string { return filepath.Join(s.St.CacheDir(), name) }

func (s *Service) Libraries(ctx context.Context) ([]abs.Library, error) {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	libs, err := s.Client.Libraries(ctx)
	s.noteNetErr(err)
	if err != nil {
		var cached []abs.Library
		if store.ReadJSON(s.cachePath("libraries.json"), &cached) == nil {
			return cached, nil
		}
		return nil, err
	}
	_ = store.WriteJSON(s.cachePath("libraries.json"), libs)
	return libs, nil
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

// Library returns every book (or podcast) in a library. Results are cached so
// the list still opens offline.
func (s *Service) Library(ctx context.Context, libraryID, sortBy string) ([]Entry, bool, error) {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	sk, ok := sortKeys[sortBy]
	if !ok {
		sk, sortBy = sortKeys["title"], "title"
	}
	cacheFile := s.cachePath("library-" + sanitizeID(libraryID) + "-" + sortBy + ".json")
	all, err := s.allItems(ctx, libraryID, abs.ItemsQuery{Sort: sk.key, Desc: sk.desc})
	if err != nil {
		var cached []abs.LibraryItem
		if store.ReadJSON(cacheFile, &cached) == nil {
			return s.entries(ctx, cached), true, nil
		}
		return nil, false, err
	}
	_ = store.WriteJSON(cacheFile, all)
	return s.entries(ctx, all), false, nil
}

func (s *Service) allItems(ctx context.Context, libraryID string, q abs.ItemsQuery) ([]abs.LibraryItem, error) {
	var all []abs.LibraryItem
	q.Limit = 250
	for q.Page = 0; ; q.Page++ {
		p, err := s.Client.LibraryItems(ctx, libraryID, q)
		s.noteNetErr(err)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		if len(p.Results) < q.Limit || len(all) >= p.Total {
			return all, nil
		}
	}
}

func (s *Service) ContinueListening(ctx context.Context) ([]Entry, error) {
	items, err := s.Client.ItemsInProgress(ctx)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	return s.entries(ctx, items), nil
}

func (s *Service) RecentEpisodes(ctx context.Context, libraryID string) ([]Entry, error) {
	eps, err := s.Client.RecentEpisodes(ctx, libraryID)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, ep := range eps {
		title := ""
		if ep.Podcast != nil {
			title = ep.Podcast.Metadata.Title
		}
		out = append(out, episodeEntry(ep.LibraryItemID, title, ep))
	}
	return s.finish(ctx, out), nil
}

func (s *Service) SeriesList(ctx context.Context, libraryID string) ([]Entry, error) {
	series, err := s.Client.Series(ctx, libraryID)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(series))
	for _, se := range series {
		e := Entry{Kind: "series", ID: se.ID, Title: se.Name, Count: len(se.Books), Library: libraryID}
		if len(se.Books) > 0 {
			e.Subtitle = se.Books[0].Author()
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Service) SeriesItems(ctx context.Context, libraryID, seriesID string) ([]Entry, error) {
	items, err := s.allItems(ctx, libraryID, abs.ItemsQuery{Sort: "sequence", Filter: abs.SeriesFilter(seriesID)})
	if err != nil {
		return nil, err
	}
	es := s.entries(ctx, items)
	for i, it := range items {
		for _, sr := range it.Media.Metadata.Series {
			if sr.ID == seriesID && sr.Sequence != "" {
				es[i].Subtitle = "Book " + sr.Sequence
			}
		}
	}
	return es, nil
}

func (s *Service) Authors(ctx context.Context, libraryID string) ([]Entry, error) {
	authors, err := s.Client.Authors(ctx, libraryID)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(authors))
	for _, a := range authors {
		out = append(out, Entry{Kind: "author", ID: a.ID, Title: a.Name, Count: a.NumBooks, Library: libraryID})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

func (s *Service) AuthorItems(ctx context.Context, authorID string) ([]Entry, error) {
	a, err := s.Client.Author(ctx, authorID)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	items := a.LibraryItems
	sort.SliceStable(items, func(i, j int) bool {
		return strings.ToLower(items[i].Media.Metadata.Title) < strings.ToLower(items[j].Media.Metadata.Title)
	})
	return s.entries(ctx, items), nil
}

func (s *Service) Collections(ctx context.Context, libraryID string) ([]Entry, error) {
	cols, err := s.Client.Collections(ctx, libraryID)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(cols))
	for _, c := range cols {
		out = append(out, Entry{Kind: "collection", ID: c.ID, Title: c.Name, Count: len(c.Books), Library: libraryID})
	}
	return out, nil
}

func (s *Service) CollectionItems(ctx context.Context, id string) ([]Entry, error) {
	c, err := s.Client.Collection(ctx, id)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	return s.entries(ctx, c.Books), nil
}

func (s *Service) Playlists(ctx context.Context, libraryID string) ([]Entry, error) {
	pls, err := s.Client.Playlists(ctx, libraryID)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(pls))
	for _, p := range pls {
		out = append(out, Entry{Kind: "playlist", ID: p.ID, Title: p.Name, Count: len(p.Items), Library: libraryID})
	}
	return out, nil
}

func (s *Service) PlaylistItems(ctx context.Context, id string) ([]Entry, error) {
	pl, err := s.Client.Playlist(ctx, id)
	s.noteNetErr(err)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, pi := range pl.Items {
		switch {
		case pi.Episode != nil:
			title := ""
			if pi.LibraryItem != nil {
				title = pi.LibraryItem.Media.Metadata.Title
			}
			out = append(out, episodeEntry(pi.LibraryItemID, title, *pi.Episode))
		case pi.LibraryItem != nil:
			out = append(out, bookEntry(*pi.LibraryItem))
		}
	}
	return s.finish(ctx, out), nil
}

// Search returns authors and series first, then books, podcasts and episodes.
// Offline it searches downloads by title.
func (s *Service) Search(ctx context.Context, libraryID, q string) ([]Entry, error) {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	r, err := s.Client.Search(ctx, libraryID, q)
	s.noteNetErr(err)
	if err != nil {
		var out []Entry
		ql := strings.ToLower(q)
		for _, d := range s.Downloaded() {
			if strings.Contains(strings.ToLower(d.Title+" "+d.Subtitle), ql) {
				out = append(out, d)
			}
		}
		if out != nil {
			return out, nil
		}
		return nil, err
	}
	var out []Entry
	for _, a := range r.Authors {
		out = append(out, Entry{Kind: "author", ID: a.ID, Title: a.Name, Subtitle: "Author", Count: a.NumBooks, Library: libraryID})
	}
	for _, se := range r.Series {
		out = append(out, Entry{Kind: "series", ID: se.Series.ID, Title: se.Series.Name, Subtitle: "Series", Count: len(se.Books), Library: libraryID})
	}
	var items []abs.LibraryItem
	for _, h := range r.Book {
		items = append(items, h.LibraryItem)
	}
	for _, h := range r.Podcast {
		items = append(items, h.LibraryItem)
	}
	for _, h := range r.Episodes {
		items = append(items, h.LibraryItem)
	}
	return append(out, s.entries(ctx, items)...), nil
}

func (s *Service) Downloaded() []Entry {
	out := []Entry{}
	for _, m := range s.DL.Completed() {
		e := Entry{Kind: "book", ID: m.ItemID, Title: m.Title, Subtitle: m.Author, Duration: m.Duration, Downloaded: true}
		if m.EpisodeID != "" {
			e.Kind, e.EpisodeID = "episode", m.EpisodeID
		}
		if lp, ok := s.St.GetProgress(m.Key()); ok && m.Duration > 0 {
			e.Progress = lp.CurrentTime / m.Duration
			e.Finished = lp.Finished
		}
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// Details

// Podcast returns a show and its episodes, newest first. Offline it lists
// the downloaded episodes.
func (s *Service) Podcast(ctx context.Context, id string) (*PodcastDetail, error) {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	it, err := s.Client.Item(ctx, id)
	s.noteNetErr(err)
	if err != nil {
		pd := &PodcastDetail{Entry: Entry{Kind: "podcast", ID: id}, Cached: true, Cover: s.DL.CoverFor(id)}
		for _, m := range s.DL.Completed() {
			if m.ItemID == id && m.EpisodeID != "" {
				pd.Title = m.Author
				e := Entry{Kind: "episode", ID: id, EpisodeID: m.EpisodeID, Title: m.Title, Duration: m.Duration}
				if m.PublishedAt > 0 {
					e.Subtitle = time.UnixMilli(m.PublishedAt).UTC().Format("2006-01-02")
				}
				pd.Episodes = append(pd.Episodes, e)
			}
		}
		if len(pd.Episodes) == 0 {
			return nil, err
		}
		pd.Episodes = s.finish(ctx, pd.Episodes)
		return pd, nil
	}
	md := it.Media.Metadata
	pd := &PodcastDetail{Entry: podcastEntry(*it), Author: it.Author(), Description: md.Description}
	eps := append([]abs.Episode(nil), it.Media.Episodes...)
	sort.SliceStable(eps, func(i, j int) bool { return eps[i].PublishedAt > eps[j].PublishedAt })
	for _, ep := range eps {
		pd.Episodes = append(pd.Episodes, episodeEntry(id, "", ep))
	}
	pd.Episodes = s.finish(ctx, pd.Episodes)
	pd.Cover = s.EnsureCover(ctx, id)
	return pd, nil
}

func (s *Service) Detail(ctx context.Context, id, episodeID string) (*Detail, error) {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	man := s.DL.Get(id, episodeID)
	it, err := s.Client.Item(ctx, id)
	s.noteNetErr(err)
	var d Detail
	switch {
	case err == nil && episodeID != "":
		var ep *abs.Episode
		for i := range it.Media.Episodes {
			if it.Media.Episodes[i].ID == episodeID {
				ep = &it.Media.Episodes[i]
			}
		}
		if ep == nil {
			return nil, abs.ErrNotFound
		}
		d.Entry = episodeEntry(id, it.Media.Metadata.Title, *ep)
		d.Author = it.Media.Metadata.Title
		d.Description = ep.Description
		d.Published = episodeDate(*ep)
		for _, c := range ep.Chapters {
			d.Chapters = append(d.Chapters, player.Chapter{Start: c.Start, End: c.End, Title: c.Title})
		}
		d.NumTracks = 1
	case err == nil:
		md := it.Media.Metadata
		d.Entry = bookEntry(*it)
		d.Author = it.Author()
		d.Description, d.Year = md.Description, md.PublishedYear
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
	case man != nil:
		kind := "book"
		if episodeID != "" {
			kind = "episode"
		}
		d.Entry = Entry{Kind: kind, ID: man.ItemID, EpisodeID: man.EpisodeID, Title: man.Title, Duration: man.Duration}
		d.Author, d.Narrator, d.Series, d.Description, d.Year = man.Author, man.Narrator, man.Series, man.Description, man.Year
		d.Chapters = man.Chapters
		d.NumTracks = len(man.Tracks)
	default:
		return nil, err
	}

	key := downloads.Key(id, episodeID)
	if p, ok := s.progressMap(ctx)[key]; ok {
		d.CurrentTime, d.Progress, d.Finished = p.CurrentTime, p.Progress, p.IsFinished
	}
	if lp, ok := s.St.GetProgress(key); ok && (lp.Dirty || err != nil) {
		d.CurrentTime = lp.CurrentTime
		if d.Duration > 0 {
			d.Progress = d.CurrentTime / d.Duration
		}
		if err != nil {
			d.Finished = lp.Finished
		}
	}
	d.Downloaded = man != nil
	if d.Chapters == nil {
		d.Chapters = []player.Chapter{}
	}
	d.Cover = s.EnsureCover(ctx, id)
	if j := s.DL.JobFor(id, episodeID); j != nil {
		d.Download = j
	}
	return &d, nil
}

// ---------------------------------------------------------------------------
// Downloads

func (s *Service) QueueDownload(ctx context.Context, id, episodeID string) error {
	if !s.Client.LoggedIn() {
		return abs.ErrNotLoggedIn
	}
	if err := os.MkdirAll(s.St.GetSettings().DownloadsDir, 0o755); err != nil {
		return err
	}
	s.DL.Enqueue(id, episodeID, "Preparing download...")
	return nil
}

func (s *Service) DeleteDownload(id, episodeID string) error {
	s.mu.Lock()
	playing := s.cur != nil && s.cur.itemID == id && s.cur.episodeID == episodeID && s.cur.isLocalFiles
	s.mu.Unlock()
	if playing {
		return errors.New("stop playback before deleting this download")
	}
	return s.DL.Delete(id, episodeID)
}

// ---------------------------------------------------------------------------
// Bookmarks (books only, as on the server)

type BookmarkView struct {
	Time    float64 `json:"time"`
	Title   string  `json:"title"`
	Pending bool    `json:"pending,omitempty"`
}

func (s *Service) cacheMe(me *abs.Me) {
	_ = store.WriteJSON(s.cachePath("bookmarks.json"), me.Bookmarks)
}

// Bookmarks lists a book's bookmarks: the server's, plus ones made offline
// that haven't been uploaded yet.
func (s *Service) Bookmarks(ctx context.Context, itemID string) []BookmarkView {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	var server []abs.Bookmark
	if me, err := s.Client.Me(ctx); err == nil {
		s.noteNetErr(nil)
		s.cacheMe(me)
		server = me.Bookmarks
	} else {
		s.noteNetErr(err)
		_ = store.ReadJSON(s.cachePath("bookmarks.json"), &server)
	}
	out := []BookmarkView{}
	for _, b := range server {
		if b.LibraryItemID == itemID {
			out = append(out, BookmarkView{Time: b.Time, Title: b.Title})
		}
	}
	for _, b := range s.St.PendingBookmarks() {
		if b.LibraryItemID == itemID {
			out = append(out, BookmarkView{Time: b.Time, Title: b.Title, Pending: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// AddBookmark marks the current position of the playing book. Offline, it is
// kept on the device and uploaded later.
func (s *Service) AddBookmark(ctx context.Context, title string) (*BookmarkView, error) {
	ctx, cancel := s.netCtx(ctx)
	defer cancel()
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		return nil, errors.New("nothing is playing")
	}
	if cur.episodeID != "" {
		return nil, errors.New("bookmarks are only available for books")
	}
	pos := float64(int(s.Eng.Position()))
	if title == "" {
		title = "Bookmark at " + clock(pos)
		if _, ch := s.chapterAt(pos); ch != nil && ch.Title != "" {
			title = fmt.Sprintf("%s, %s", ch.Title, clock(pos-ch.Start))
		}
	}
	err := s.Client.AddBookmark(ctx, cur.itemID, pos, title)
	s.noteNetErr(err)
	if err != nil {
		var he *abs.HTTPError
		if errors.As(err, &he) || errors.Is(err, abs.ErrAuth) {
			return nil, err
		}
		s.St.AddPendingBookmark(abs.Bookmark{LibraryItemID: cur.itemID, Time: pos, Title: title, CreatedAt: store.NowMs()})
		return &BookmarkView{Time: pos, Title: title, Pending: true}, nil
	}
	return &BookmarkView{Time: pos, Title: title}, nil
}

func (s *Service) DeleteBookmark(ctx context.Context, itemID string, t float64) error {
	if s.St.DropPendingBookmark(itemID, t) {
		return nil
	}
	err := s.Client.DeleteBookmark(ctx, itemID, t)
	s.noteNetErr(err)
	return err
}

func (s *Service) flushBookmarks() {
	for _, b := range s.St.PendingBookmarks() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.Client.AddBookmark(ctx, b.LibraryItemID, b.Time, b.Title)
		cancel()
		if err != nil {
			return
		}
		s.St.DropPendingBookmark(b.LibraryItemID, b.Time)
	}
}

func clock(sec float64) string {
	t := int(sec)
	if t >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
	}
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}
