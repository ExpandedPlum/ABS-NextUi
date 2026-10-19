package service

import (
	"os"
	"sort"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/player"
	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/store"
)

type PlayerStatus struct {
	State        string  `json:"state"`
	ItemID       string  `json:"itemId,omitempty"`
	EpisodeID    string  `json:"episodeId,omitempty"`
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
	Version   string          `json:"version"`
	LoggedIn  bool            `json:"loggedIn"`
	Server    string          `json:"server,omitempty"`
	Username  string          `json:"username,omitempty"`
	Online    bool            `json:"online"`
	LibraryID string          `json:"libraryId,omitempty"`
	Phone     *PhoneLogin     `json:"phoneLogin,omitempty"`
	GameAudio GameAudioStatus `json:"gameAudio"`
	Player    PlayerStatus    `json:"player"`
	Downloads []any           `json:"downloads"`
	Settings  store.Settings  `json:"settings"`
	Pending   int             `json:"pendingSyncs"`
	Time      int64           `json:"time"`
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
		ps.ItemID, ps.EpisodeID, ps.Title, ps.Author = cur.itemID, cur.episodeID, cur.media.Title, cur.media.Author
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
	st.Phone = s.phoneStatus()
	st.GameAudio = s.gameAudioStatus()
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
