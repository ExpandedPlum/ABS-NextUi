// Package downloads stores books on the SD card for offline listening.
//
// Each book lives in its own folder with the audio files, cover.jpg and a
// manifest.json describing tracks and chapters. A folder without a complete
// manifest is treated as an unfinished download.
package downloads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/store"
)

type ManifestTrack struct {
	StartOffset float64 `json:"startOffset"`
	Duration    float64 `json:"duration"`
	File        string  `json:"file"`
	Size        int64   `json:"size"`
}

type Manifest struct {
	ItemID       string           `json:"itemId"`
	EpisodeID    string           `json:"episodeId,omitempty"`
	Title        string           `json:"title"`
	Subtitle     string           `json:"subtitle,omitempty"`
	Author       string           `json:"author"`
	Narrator     string           `json:"narrator,omitempty"`
	Series       string           `json:"series,omitempty"`
	Description  string           `json:"description,omitempty"`
	Year         string           `json:"year,omitempty"`
	Duration     float64          `json:"duration"`
	Chapters     []player.Chapter `json:"chapters"`
	Tracks       []ManifestTrack  `json:"tracks"`
	DownloadedAt int64            `json:"downloadedAt"`
	PublishedAt  int64            `json:"publishedAt,omitempty"`

	Dir string `json:"-"`
}

func (m *Manifest) CoverPath() string { return filepath.Join(m.Dir, "cover.jpg") }

func (m *Manifest) Key() string { return Key(m.ItemID, m.EpisodeID) }

// Key identifies a download: a book, or one episode of a podcast.
func Key(itemID, episodeID string) string {
	if episodeID == "" {
		return itemID
	}
	return itemID + "|" + episodeID
}

func (m *Manifest) Media() *player.Media {
	pm := &player.Media{ItemID: m.ItemID, EpisodeID: m.EpisodeID, Title: m.Title, Author: m.Author, Duration: m.Duration, Chapters: m.Chapters}
	for _, t := range m.Tracks {
		pm.Tracks = append(pm.Tracks, player.Track{StartOffset: t.StartOffset, Duration: t.Duration, Source: filepath.Join(m.Dir, t.File)})
	}
	return pm
}

type Job struct {
	ItemID    string  `json:"itemId"`
	EpisodeID string  `json:"episodeId,omitempty"`
	Title     string  `json:"title"`
	State     string  `json:"state"` // queued, downloading, error
	Progress  float64 `json:"progress"`
	Error     string  `json:"error,omitempty"`
}

type Manager struct {
	client *abs.Client
	st     *store.Store

	mu       sync.Mutex
	done     map[string]*Manifest
	jobs     []*Job
	cancel   context.CancelFunc
	activeID string
	wake     chan struct{}
}

func New(client *abs.Client, st *store.Store) *Manager {
	m := &Manager{client: client, st: st, done: map[string]*Manifest{}, wake: make(chan struct{}, 1)}
	m.Rescan()
	go m.worker()
	return m
}

func (m *Manager) root() string { return m.st.GetSettings().DownloadsDir }

// Rescan reloads completed downloads from disk.
func (m *Manager) Rescan() {
	found := map[string]*Manifest{}
	dirs, _ := filepath.Glob(filepath.Join(m.root(), "*", "manifest.json"))
	eps, _ := filepath.Glob(filepath.Join(m.root(), "Podcasts", "*", "*", "manifest.json"))
	for _, mf := range append(dirs, eps...) {
		var man Manifest
		if err := store.ReadJSON(mf, &man); err != nil || man.ItemID == "" {
			continue
		}
		man.Dir = filepath.Dir(mf)
		found[man.Key()] = &man
	}
	m.mu.Lock()
	m.done = found
	m.mu.Unlock()
}

func (m *Manager) Get(id, episodeID string) *Manifest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.done[Key(id, episodeID)]
}

// CoverFor returns a downloaded cover for a book or podcast, if any.
func (m *Manager) CoverFor(itemID string) string {
	m.mu.Lock()
	var dirs []string
	for _, man := range m.done {
		if man.ItemID == itemID {
			dirs = append(dirs, man.CoverPath())
		}
	}
	m.mu.Unlock()
	for _, p := range dirs {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func (m *Manager) Completed() []*Manifest {
	m.mu.Lock()
	out := make([]*Manifest, 0, len(m.done))
	for _, v := range m.done {
		out = append(out, v)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, len(m.jobs))
	for i, j := range m.jobs {
		out[i] = *j
	}
	return out
}

func (m *Manager) JobFor(id, episodeID string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.ItemID == id && j.EpisodeID == episodeID {
			c := *j
			return &c
		}
	}
	return nil
}

func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.State != "error" {
			return true
		}
	}
	return false
}

func (m *Manager) Enqueue(id, episodeID, title string) {
	m.mu.Lock()
	if _, ok := m.done[Key(id, episodeID)]; ok {
		m.mu.Unlock()
		return
	}
	for _, j := range m.jobs {
		if j.ItemID == id && j.EpisodeID == episodeID {
			if j.State == "error" {
				j.State, j.Error = "queued", ""
			}
			m.mu.Unlock()
			m.kick()
			return
		}
	}
	m.jobs = append(m.jobs, &Job{ItemID: id, EpisodeID: episodeID, Title: title, State: "queued"})
	m.mu.Unlock()
	m.kick()
}

// Cancel removes a queued or running job and its partial files.
func (m *Manager) Cancel(id, episodeID string) {
	key := Key(id, episodeID)
	m.mu.Lock()
	if m.activeID == key && m.cancel != nil {
		m.cancel()
	}
	for i, j := range m.jobs {
		if j.ItemID == id && j.EpisodeID == episodeID {
			m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
}

func (m *Manager) Delete(id, episodeID string) error {
	m.Cancel(id, episodeID)
	key := Key(id, episodeID)
	m.mu.Lock()
	man := m.done[key]
	delete(m.done, key)
	m.mu.Unlock()
	if man == nil {
		return nil
	}
	return os.RemoveAll(man.Dir)
}

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) next() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.State == "queued" {
			return j
		}
	}
	return nil
}

func (m *Manager) worker() {
	for {
		j := m.next()
		if j == nil {
			<-m.wake
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.mu.Lock()
		j.State = "downloading"
		m.cancel, m.activeID = cancel, Key(j.ItemID, j.EpisodeID)
		m.mu.Unlock()

		man, err := m.download(ctx, j)
		cancel()

		m.mu.Lock()
		m.cancel, m.activeID = nil, ""
		if err == nil {
			m.done[man.Key()] = man
			for i, x := range m.jobs {
				if x == j {
					m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
					break
				}
			}
		} else if !errors.Is(err, context.Canceled) {
			log.Printf("download %s: %v", j.ItemID, err)
			j.State, j.Error = "error", err.Error()
		}
		m.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			time.Sleep(time.Second)
		}
	}
}

var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitize(s string) string {
	s = unsafeChars.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, ". ")
	if len(s) > 80 {
		s = strings.TrimSpace(s[:80])
	}
	if s == "" {
		s = "Untitled"
	}
	return s
}

func (m *Manager) setProgress(j *Job, p float64) {
	m.mu.Lock()
	j.Progress = p
	m.mu.Unlock()
}

func (m *Manager) download(ctx context.Context, j *Job) (*Manifest, error) {
	item, err := m.client.Item(ctx, j.ItemID)
	if err != nil {
		return nil, err
	}
	md := item.Media.Metadata
	short := func(id string) string {
		if len(id) > 8 {
			return id[len(id)-8:]
		}
		return id
	}

	var man *Manifest
	var tracks []abs.AudioTrack
	var folder string
	if j.EpisodeID != "" {
		var ep *abs.Episode
		for i := range item.Media.Episodes {
			if item.Media.Episodes[i].ID == j.EpisodeID {
				ep = &item.Media.Episodes[i]
			}
		}
		if ep == nil || ep.AudioTrack == nil {
			return nil, errors.New("episode has no audio")
		}
		tracks = []abs.AudioTrack{*ep.AudioTrack}
		tracks[0].StartOffset = 0
		if tracks[0].Duration <= 0 {
			tracks[0].Duration = ep.Length()
		}
		man = &Manifest{
			ItemID: j.ItemID, EpisodeID: j.EpisodeID, Title: ep.Title, Subtitle: ep.Subtitle,
			Author: md.Title, Description: ep.Description, Duration: ep.Length(), PublishedAt: ep.PublishedAt,
		}
		for _, c := range ep.Chapters {
			man.Chapters = append(man.Chapters, player.Chapter{Start: c.Start, End: c.End, Title: c.Title})
		}
		folder = filepath.Join("Podcasts", sanitize(md.Title)+" ["+short(j.ItemID)+"]", sanitize(ep.Title)+" ["+short(j.EpisodeID)+"]")
	} else {
		if len(item.Media.Tracks) == 0 {
			return nil, errors.New("book has no audio tracks")
		}
		tracks = item.Media.Tracks
		man = &Manifest{
			ItemID: j.ItemID, Title: md.Title, Subtitle: md.Subtitle, Author: item.Author(),
			Narrator: md.NarratorName, Series: md.SeriesName, Description: md.Description,
			Year: md.PublishedYear, Duration: item.Media.Duration,
		}
		if man.Narrator == "" {
			man.Narrator = strings.Join(md.Narrators, ", ")
		}
		if man.Series == "" && len(md.Series) > 0 {
			man.Series = md.Series[0].Name
			if md.Series[0].Sequence != "" {
				man.Series += " #" + md.Series[0].Sequence
			}
		}
		for _, c := range item.Media.Chapters {
			man.Chapters = append(man.Chapters, player.Chapter{Start: c.Start, End: c.End, Title: c.Title})
		}
		folder = sanitize(md.Title)
		if a := item.Author(); a != "" {
			folder = sanitize(a) + " - " + folder
		}
		folder += " [" + short(j.ItemID) + "]"
	}
	m.mu.Lock()
	j.Title = man.Title
	m.mu.Unlock()

	dir := filepath.Join(m.root(), folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	man.Dir = dir

	var total, got int64
	for _, t := range tracks {
		total += t.Metadata.Size
	}
	for i, t := range tracks {
		ext := t.Metadata.Ext
		if ext == "" {
			ext = filepath.Ext(t.Metadata.Filename)
		}
		name := fmt.Sprintf("%03d%s", i+1, strings.ToLower(ext))
		n, err := m.fetchFile(ctx, t.ContentURL, filepath.Join(dir, name), t.Metadata.Size, func(done int64) {
			if total > 0 {
				m.setProgress(j, float64(got+done)/float64(total))
			} else {
				m.setProgress(j, float64(i)/float64(len(tracks)))
			}
		})
		if err != nil {
			return nil, err
		}
		got += n
		man.Tracks = append(man.Tracks, ManifestTrack{StartOffset: t.StartOffset, Duration: t.Duration, File: name, Size: n})
	}
	if err := m.fetchCover(ctx, j.ItemID, filepath.Join(dir, "cover.jpg")); err != nil {
		log.Printf("download %s: cover: %v", j.ItemID, err)
	}
	man.DownloadedAt = store.NowMs()
	if err := store.WriteJSON(filepath.Join(dir, "manifest.json"), man); err != nil {
		return nil, err
	}
	return man, nil
}

// fetchFile downloads to dest, resuming a previous partial download.
func (m *Manager) fetchFile(ctx context.Context, path, dest string, size int64, progress func(int64)) (int64, error) {
	if fi, err := os.Stat(dest); err == nil && size > 0 && fi.Size() == size {
		progress(size)
		return size, nil
	}
	part := dest + ".part"
	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	hdr := http.Header{}
	if offset > 0 {
		hdr.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := m.client.Open(ctx, http.MethodGet, path, hdr, nil, true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return 0, &abs.HTTPError{Status: resp.StatusCode}
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return 0, err
	}
	written := offset
	buf := make([]byte, 256<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return 0, werr
			}
			written += int64(n)
			if time.Since(last) > 300*time.Millisecond {
				progress(written)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return 0, rerr
		}
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if size > 0 && written != size {
		return 0, fmt.Errorf("incomplete download (%d of %d bytes)", written, size)
	}
	progress(written)
	return written, os.Rename(part, dest)
}

func (m *Manager) fetchCover(ctx context.Context, id, dest string) error {
	resp, err := m.client.Open(ctx, http.MethodGet, abs.CoverPath(id, 600), nil, nil, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &abs.HTTPError{Status: resp.StatusCode}
	}
	f, err := os.Create(dest + ".tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(dest+".tmp", dest)
}
