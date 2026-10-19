package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ExpandedPlum/ABS-NextUi/daemon/internal/abs"
)

// ServeControl exposes a small JSON API on a unix socket for the UI and the
// `absd ctl` command. Every endpoint is a POST or GET returning JSON;
// failures come back as {"error": "..."} with a 4xx/5xx status.
func (s *Service) ServeControl(socketPath string) error {
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(socketPath, 0o666)
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-s.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type req struct {
	ID       string           `json:"id"`
	Episode  string           `json:"episodeId"`
	APIKey   string           `json:"apiKey"`
	Title    string           `json:"title"`
	Time     float64          `json:"time"`
	Library  string           `json:"library"`
	Query    string           `json:"q"`
	Sort     string           `json:"sort"`
	Position *float64         `json:"position"`
	Seconds  float64          `json:"seconds"`
	Index    int              `json:"index"`
	Speed    float64          `json:"speed"`
	Minutes  float64          `json:"minutes"`
	Finished bool             `json:"finished"`
	Server   string           `json:"server"`
	Username string           `json:"username"`
	Password string           `json:"password"`
	Fg       bool             `json:"foreground"`
	Settings *json.RawMessage `json:"settings"`
}

func (s *Service) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(path string, fn func(ctx context.Context, r req) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, hr *http.Request) {
			var r req
			if hr.Method == http.MethodPost {
				body, _ := io.ReadAll(io.LimitReader(hr.Body, 1<<20))
				if len(body) > 0 {
					if err := json.Unmarshal(body, &r); err != nil {
						writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
						return
					}
				}
			}
			q := hr.URL.Query()
			if v := q.Get("id"); v != "" {
				r.ID = v
			}
			if v := q.Get("library"); v != "" {
				r.Library = v
			}
			if v := q.Get("q"); v != "" {
				r.Query = v
			}
			if v := q.Get("sort"); v != "" {
				r.Sort = v
			}
			ctx, cancel := context.WithTimeout(hr.Context(), 60*time.Second)
			defer cancel()
			out, err := fn(ctx, r)
			if err != nil {
				code := http.StatusInternalServerError
				if errors.Is(err, abs.ErrAuth) || errors.Is(err, abs.ErrNotLoggedIn) {
					code = http.StatusUnauthorized
				}
				log.Printf("%s: %v", path, err)
				writeJSON(w, code, map[string]string{"error": userError(err)})
				return
			}
			if out == nil {
				out = map[string]bool{"ok": true}
			}
			writeJSON(w, http.StatusOK, out)
		})
	}

	h("/status", func(ctx context.Context, r req) (any, error) { return s.Status(), nil })
	h("/ui", func(ctx context.Context, r req) (any, error) { s.UIHeartbeat(r.Fg); return nil, nil })
	h("/check", func(ctx context.Context, r req) (any, error) {
		return map[string]bool{"online": s.CheckOnline(ctx)}, nil
	})
	h("/login", func(ctx context.Context, r req) (any, error) {
		var err error
		if r.APIKey != "" {
			err = s.Client.LoginAPIKey(ctx, r.Server, r.APIKey)
		} else {
			err = s.Client.Login(ctx, r.Server, r.Username, r.Password)
		}
		s.noteNetErr(err)
		if err != nil {
			return nil, err
		}
		s.afterLogin()
		return s.Status(), nil
	})
	h("/logout", func(ctx context.Context, r req) (any, error) {
		s.Stop()
		s.Client.Logout()
		return nil, nil
	})
	h("/libraries", func(ctx context.Context, r req) (any, error) { return s.Libraries(ctx) })
	h("/continue", func(ctx context.Context, r req) (any, error) { return s.ContinueListening(ctx) })
	h("/library", func(ctx context.Context, r req) (any, error) {
		items, cached, err := s.Library(ctx, r.Library, r.Sort)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items, "cached": cached}, nil
	})
	h("/search", func(ctx context.Context, r req) (any, error) { return s.Search(ctx, r.Library, r.Query) })
	h("/episodes/recent", func(ctx context.Context, r req) (any, error) { return s.RecentEpisodes(ctx, r.Library) })
	h("/podcast", func(ctx context.Context, r req) (any, error) { return s.Podcast(ctx, r.ID) })
	h("/series", func(ctx context.Context, r req) (any, error) { return s.SeriesList(ctx, r.Library) })
	h("/series/items", func(ctx context.Context, r req) (any, error) { return s.SeriesItems(ctx, r.Library, r.ID) })
	h("/authors", func(ctx context.Context, r req) (any, error) { return s.Authors(ctx, r.Library) })
	h("/author/items", func(ctx context.Context, r req) (any, error) { return s.AuthorItems(ctx, r.ID) })
	h("/collections", func(ctx context.Context, r req) (any, error) { return s.Collections(ctx, r.Library) })
	h("/collection/items", func(ctx context.Context, r req) (any, error) { return s.CollectionItems(ctx, r.ID) })
	h("/playlists", func(ctx context.Context, r req) (any, error) { return s.Playlists(ctx, r.Library) })
	h("/playlist/items", func(ctx context.Context, r req) (any, error) { return s.PlaylistItems(ctx, r.ID) })
	h("/bookmarks", func(ctx context.Context, r req) (any, error) { return s.Bookmarks(ctx, r.ID), nil })
	h("/bookmark/add", func(ctx context.Context, r req) (any, error) { return s.AddBookmark(ctx, r.Title) })
	h("/bookmark/delete", func(ctx context.Context, r req) (any, error) { return nil, s.DeleteBookmark(ctx, r.ID, r.Time) })
	h("/login/phone", func(ctx context.Context, r req) (any, error) { return s.StartPhoneLogin() })
	h("/login/phone/stop", func(ctx context.Context, r req) (any, error) { s.StopPhoneLogin(); return nil, nil })
	h("/downloaded", func(ctx context.Context, r req) (any, error) {
		return map[string]any{"items": s.Downloaded(), "jobs": s.DL.Jobs()}, nil
	})
	h("/item", func(ctx context.Context, r req) (any, error) { return s.Detail(ctx, r.ID, r.Episode) })
	h("/cover", func(ctx context.Context, r req) (any, error) {
		return map[string]string{"path": s.EnsureCover(ctx, r.ID)}, nil
	})

	h("/play", func(ctx context.Context, r req) (any, error) { return nil, s.Play(ctx, r.ID, r.Episode, r.Position) })
	h("/pause", func(ctx context.Context, r req) (any, error) { s.Pause(); return nil, nil })
	h("/resume", func(ctx context.Context, r req) (any, error) { return nil, s.Resume() })
	h("/toggle", func(ctx context.Context, r req) (any, error) { return nil, s.Toggle() })
	h("/stop", func(ctx context.Context, r req) (any, error) { s.Stop(); return nil, nil })
	h("/seek", func(ctx context.Context, r req) (any, error) {
		if r.Position == nil {
			return nil, errors.New("position required")
		}
		return nil, s.Seek(*r.Position)
	})
	h("/skip", func(ctx context.Context, r req) (any, error) { return nil, s.Skip(r.Seconds) })
	h("/chapter", func(ctx context.Context, r req) (any, error) { return nil, s.SeekChapter(r.Index) })
	h("/chapter/next", func(ctx context.Context, r req) (any, error) { return nil, s.NextChapter() })
	h("/chapter/prev", func(ctx context.Context, r req) (any, error) { return nil, s.PrevChapter() })
	h("/chapters", func(ctx context.Context, r req) (any, error) { return s.Chapters(), nil })
	h("/speed", func(ctx context.Context, r req) (any, error) { return nil, s.SetSpeed(r.Speed) })
	h("/sleep", func(ctx context.Context, r req) (any, error) { s.SetSleep(r.Minutes); return nil, nil })
	h("/finished", func(ctx context.Context, r req) (any, error) { return nil, s.MarkFinished(r.ID, r.Episode, r.Finished) })
	h("/sync", func(ctx context.Context, r req) (any, error) { s.Sync(false); s.flushPending(); return nil, nil })
	h("/gameaudio/toggle", func(ctx context.Context, r req) (any, error) { s.Hotkey("gameAudio"); return s.gameAudioStatus(), nil })
	h("/presleep", func(ctx context.Context, r req) (any, error) { s.PreSleep(); return nil, nil })

	h("/download", func(ctx context.Context, r req) (any, error) { return nil, s.QueueDownload(ctx, r.ID, r.Episode) })
	h("/download/cancel", func(ctx context.Context, r req) (any, error) { s.DL.Cancel(r.ID, r.Episode); return nil, nil })
	h("/download/delete", func(ctx context.Context, r req) (any, error) { return nil, s.DeleteDownload(r.ID, r.Episode) })

	h("/settings", func(ctx context.Context, r req) (any, error) {
		if r.Settings != nil {
			set := s.St.GetSettings()
			if err := json.Unmarshal(*r.Settings, &set); err != nil {
				return nil, err
			}
			if err := s.St.SaveSettings(set); err != nil {
				return nil, err
			}
			if s.onSettings != nil {
				s.onSettings(set)
			}
			s.DL.Rescan()
		}
		return s.St.GetSettings(), nil
	})
	h("/quit", func(ctx context.Context, r req) (any, error) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			s.Shutdown()
			s.Quit()
		}()
		return nil, nil
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(v)
	if err != nil {
		code = http.StatusInternalServerError
		b = []byte(`{"error":"encoding failed"}`)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// userError turns Go errors into short messages fit for a handheld screen.
func userError(err error) string {
	var ne net.Error
	var he *abs.HTTPError
	switch {
	case errors.Is(err, abs.ErrNotLoggedIn):
		return "Not logged in"
	case errors.Is(err, abs.ErrAuth):
		return err.Error()
	case errors.Is(err, abs.ErrNotFound):
		return "Not found on server"
	case errors.Is(err, context.DeadlineExceeded):
		return "Server took too long to respond"
	case errors.As(err, &he):
		return he.Error()
	case errors.As(err, &ne):
		return "Can't reach server (check Wi-Fi and address)"
	}
	return err.Error()
}
