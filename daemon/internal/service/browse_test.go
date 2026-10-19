package service

import (
	"encoding/json"
	"testing"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
)

// Items-in-progress and episode search hits are podcast items carrying a
// recentEpisode; they must become playable episode entries.
func TestItemEntryKinds(t *testing.T) {
	var it abs.LibraryItem
	raw := `{"id":"pod1","mediaType":"podcast","media":{"metadata":{"title":"Show","author":"Host"},"numEpisodes":4},
		"recentEpisode":{"id":"ep9","title":"Ep 9","publishedAt":1767225600000,"audioFile":{"duration":120}}}`
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	e := itemEntry(it)
	if e.Kind != "episode" || e.ID != "pod1" || e.EpisodeID != "ep9" || e.Duration != 120 {
		t.Fatalf("got %+v", e)
	}
	if e.Subtitle != "Show - 2026-01-01" {
		t.Errorf("subtitle %q", e.Subtitle)
	}

	it.RecentEpisode = nil
	if e := itemEntry(it); e.Kind != "podcast" || e.Count != 4 || e.Subtitle != "Host" {
		t.Fatalf("podcast entry %+v", e)
	}

	var book abs.LibraryItem
	_ = json.Unmarshal([]byte(`{"id":"b1","mediaType":"book","media":{"metadata":{"title":"T","authorName":"A"},"duration":60},
		"userMediaProgress":{"progress":0.5}}`), &book)
	if e := itemEntry(book); e.Kind != "book" || e.Subtitle != "A" || e.Progress != 0.5 {
		t.Fatalf("book entry %+v", e)
	}
}
