// Package abs is a small Audiobookshelf API client covering what a listening
// client needs: auth (JWT access/refresh and legacy tokens), browsing,
// playback sessions, progress, and file streaming.
package abs

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	// Bundled Mozilla roots, used only when the device has no usable CA store.
	_ "golang.org/x/crypto/x509roots/fallback"
)

var (
	ErrNotLoggedIn = errors.New("not logged in")
	ErrAuth        = errors.New("authentication failed")
	ErrNotFound    = errors.New("not found")
)

// Credentials are persisted between runs.
type Credentials struct {
	Server       string `json:"server"`
	Username     string `json:"username"`
	UserID       string `json:"userId"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	LegacyToken  string `json:"token,omitempty"`
}

func (c Credentials) bearer() string {
	if c.AccessToken != "" {
		return c.AccessToken
	}
	return c.LegacyToken
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 200 {
		b = b[:200]
	}
	if b == "" {
		return fmt.Sprintf("server returned %d", e.Status)
	}
	return fmt.Sprintf("server returned %d: %s", e.Status, b)
}

type Client struct {
	mu       sync.Mutex
	creds    Credentials
	api      *http.Client // short timeouts, JSON calls
	stream   *http.Client // no overall timeout, for audio and downloads
	onUpdate func(Credentials)

	Device DeviceInfo
}

func NewClient(device DeviceInfo, insecureTLS bool, onUpdate func(Credentials)) *Client {
	newTransport := func() *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		t.TLSHandshakeTimeout = 8 * time.Second
		t.ResponseHeaderTimeout = 20 * time.Second
		if insecureTLS {
			t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		return t
	}
	return &Client{
		api:      &http.Client{Timeout: 25 * time.Second, Transport: newTransport()},
		stream:   &http.Client{Transport: newTransport()},
		onUpdate: onUpdate,
		Device:   device,
	}
}

func (c *Client) SetCredentials(cr Credentials) {
	c.mu.Lock()
	c.creds = cr
	c.mu.Unlock()
}

func (c *Client) Credentials() Credentials {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creds
}

func (c *Client) LoggedIn() bool {
	cr := c.Credentials()
	return cr.Server != "" && cr.bearer() != ""
}

// NormalizeServer turns user input like "abs.example.com" or
// "http://10.0.0.5:13378/" into a clean base URL.
func NormalizeServer(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("server address is empty")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server address %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (c *Client) Login(ctx context.Context, server, username, password string) error {
	base, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Ask newer servers (2.26+) for an access/refresh token pair.
	req.Header.Set("x-return-tokens", "true")
	resp, err := c.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: wrong username or password", ErrAuth)
	}
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	var lr loginResponse
	if err := json.Unmarshal(data, &lr); err != nil {
		return fmt.Errorf("unexpected login response: %w", err)
	}
	cr := Credentials{
		Server:       base,
		Username:     lr.User.Username,
		UserID:       lr.User.ID,
		AccessToken:  lr.User.AccessToken,
		RefreshToken: lr.User.RefreshToken,
		LegacyToken:  lr.User.Token,
	}
	if cr.bearer() == "" {
		return errors.New("server did not return a token")
	}
	c.SetCredentials(cr)
	if c.onUpdate != nil {
		c.onUpdate(cr)
	}
	return nil
}

// LoginAPIKey signs in with an API key (Settings → API Keys on the server).
// This is also how accounts that only use SSO can sign in.
func (c *Client) LoginAPIKey(ctx context.Context, server, key string) error {
	base, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	key = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "Bearer "))
	if key == "" {
		return errors.New("API key is empty")
	}
	prev := c.Credentials()
	c.SetCredentials(Credentials{Server: base, LegacyToken: key})
	me, err := c.Me(ctx)
	if err != nil {
		c.SetCredentials(prev)
		if errors.Is(err, ErrAuth) {
			return fmt.Errorf("%w: the server rejected that API key", ErrAuth)
		}
		return err
	}
	cr := Credentials{Server: base, Username: me.Username, UserID: me.ID, LegacyToken: key}
	c.SetCredentials(cr)
	if c.onUpdate != nil {
		c.onUpdate(cr)
	}
	return nil
}

// Logout drops the tokens but remembers server and username for next login.
func (c *Client) Logout() {
	old := c.Credentials()
	cr := Credentials{Server: old.Server, Username: old.Username}
	c.SetCredentials(cr)
	if c.onUpdate != nil {
		c.onUpdate(cr)
	}
}

// refresh exchanges the refresh token for a new pair. When the server does not
// support refresh but issued a legacy token, fall back to that.
func (c *Client) refresh(ctx context.Context, failedBearer string) error {
	c.mu.Lock()
	cr := c.creds
	c.mu.Unlock()
	if cr.bearer() != failedBearer {
		return nil // another request already refreshed
	}
	if cr.RefreshToken == "" {
		if cr.LegacyToken != "" && cr.AccessToken != "" {
			cr.AccessToken = ""
			c.SetCredentials(cr)
			return nil
		}
		return ErrAuth
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cr.Server+"/auth/refresh", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-refresh-token", cr.RefreshToken)
	resp, err := c.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if cr.LegacyToken != "" {
				cr.AccessToken, cr.RefreshToken = "", ""
				c.SetCredentials(cr)
				return nil
			}
			return ErrAuth
		}
		return &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	var lr loginResponse
	if err := json.Unmarshal(data, &lr); err != nil || lr.User.AccessToken == "" {
		return fmt.Errorf("unexpected refresh response")
	}
	cr.AccessToken = lr.User.AccessToken
	if lr.User.RefreshToken != "" {
		cr.RefreshToken = lr.User.RefreshToken
	}
	c.SetCredentials(cr)
	if c.onUpdate != nil {
		c.onUpdate(cr)
	}
	return nil
}

// Open performs an authenticated request against the server and returns the
// raw response. A 401 triggers one token refresh and retry. The caller must
// close the body. Used for streaming audio and downloading files.
func (c *Client) Open(ctx context.Context, method, path string, header http.Header, body []byte, streaming bool) (*http.Response, error) {
	hc := c.api
	if streaming {
		hc = c.stream
	}
	for attempt := 0; attempt < 2; attempt++ {
		cr := c.Credentials()
		if cr.Server == "" || cr.bearer() == "" {
			return nil, ErrNotLoggedIn
		}
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, cr.Server+path, rdr)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		bearer := cr.bearer()
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			if err := c.refresh(ctx, bearer); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
	return nil, ErrAuth
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	resp, err := c.Open(ctx, method, path, nil, body, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrAuth
	case resp.StatusCode/100 != 2:
		return &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

func (c *Client) Ping(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/api/me", nil, nil)
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var m Me
	if err := c.doJSON(ctx, http.MethodGet, "/api/me", nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *Client) Libraries(ctx context.Context) ([]Library, error) {
	var r librariesResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries", nil, &r)
	return r.Libraries, err
}

// ItemsQuery selects a page of library items. Sort is an Audiobookshelf
// sort key such as "media.metadata.title", "addedAt" or "sequence"; Filter is
// e.g. "series.<base64 id>".
type ItemsQuery struct {
	Page, Limit int
	Sort        string
	Desc        bool
	Filter      string
}

func (c *Client) LibraryItems(ctx context.Context, libraryID string, iq ItemsQuery) (ItemsPage, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(iq.Limit))
	q.Set("page", strconv.Itoa(iq.Page))
	q.Set("minified", "1")
	q.Set("include", "progress")
	if iq.Sort != "" {
		q.Set("sort", iq.Sort)
	}
	if iq.Desc {
		q.Set("desc", "1")
	}
	if iq.Filter != "" {
		q.Set("filter", iq.Filter)
	}
	var p ItemsPage
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/items?"+q.Encode(), nil, &p)
	return p, err
}

// SeriesFilter is the items filter selecting one series.
func SeriesFilter(seriesID string) string {
	return "series." + base64.StdEncoding.EncodeToString([]byte(seriesID))
}

func (c *Client) Search(ctx context.Context, libraryID, query string) (*SearchResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", "50")
	var r SearchResult
	if err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/search?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ItemsInProgress(ctx context.Context) ([]LibraryItem, error) {
	var r itemsInProgressResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/me/items-in-progress", nil, &r)
	return r.LibraryItems, err
}

func (c *Client) RecentEpisodes(ctx context.Context, libraryID string) ([]Episode, error) {
	var r recentEpisodesResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/recent-episodes?limit=50", nil, &r)
	return r.Episodes, err
}

// Series lists every series in a library (each with its books).
func (c *Client) Series(ctx context.Context, libraryID string) ([]Series, error) {
	var all []Series
	for page := 0; ; page++ {
		var p seriesPage
		path := fmt.Sprintf("/api/libraries/%s/series?limit=100&page=%d&minified=1&sort=name", url.PathEscape(libraryID), page)
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		if len(p.Results) < 100 || len(all) >= p.Total {
			return all, nil
		}
	}
}

func (c *Client) Authors(ctx context.Context, libraryID string) ([]Author, error) {
	var r struct {
		Authors []Author `json:"authors"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/authors?minified=1", nil, &r)
	return r.Authors, err
}

func (c *Client) Author(ctx context.Context, id string) (*Author, error) {
	var a Author
	if err := c.doJSON(ctx, http.MethodGet, "/api/authors/"+url.PathEscape(id)+"?include=items", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (c *Client) Collections(ctx context.Context, libraryID string) ([]Collection, error) {
	var p collectionsPage
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/collections?limit=500", nil, &p)
	return p.Results, err
}

func (c *Client) Collection(ctx context.Context, id string) (*Collection, error) {
	var col Collection
	if err := c.doJSON(ctx, http.MethodGet, "/api/collections/"+url.PathEscape(id), nil, &col); err != nil {
		return nil, err
	}
	return &col, nil
}

func (c *Client) Playlists(ctx context.Context, libraryID string) ([]Playlist, error) {
	var p playlistsPage
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/playlists?limit=500", nil, &p)
	return p.Results, err
}

func (c *Client) Playlist(ctx context.Context, id string) (*Playlist, error) {
	var pl Playlist
	if err := c.doJSON(ctx, http.MethodGet, "/api/playlists/"+url.PathEscape(id), nil, &pl); err != nil {
		return nil, err
	}
	return &pl, nil
}

func (c *Client) Item(ctx context.Context, id string) (*LibraryItem, error) {
	var it LibraryItem
	err := c.doJSON(ctx, http.MethodGet, "/api/items/"+url.PathEscape(id)+"?expanded=1&include=progress", nil, &it)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func progressPath(id, episodeID string) string {
	p := "/api/me/progress/" + url.PathEscape(id)
	if episodeID != "" {
		p += "/" + url.PathEscape(episodeID)
	}
	return p
}

func (c *Client) UpdateProgress(ctx context.Context, id, episodeID string, patch map[string]any) error {
	return c.doJSON(ctx, http.MethodPatch, progressPath(id, episodeID), patch, nil)
}

func (c *Client) AddBookmark(ctx context.Context, id string, time float64, title string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/me/item/"+url.PathEscape(id)+"/bookmark",
		map[string]any{"time": time, "title": title}, nil)
}

func (c *Client) DeleteBookmark(ctx context.Context, id string, time float64) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/me/item/"+url.PathEscape(id)+"/bookmark/"+
		strconv.FormatFloat(time, 'f', -1, 64), nil, nil)
}

// StartPlay opens a playback session for a book, or for one podcast episode
// when episodeID is set.
func (c *Client) StartPlay(ctx context.Context, id, episodeID string) (*PlaySession, error) {
	req := map[string]any{
		"deviceInfo":      c.Device,
		"forceDirectPlay": true,
		"forceTranscode":  false,
		"mediaPlayer":     "ffmpeg",
		"supportedMimeTypes": []string{
			"audio/flac", "audio/mpeg", "audio/mp4", "audio/ogg", "audio/aac",
			"audio/x-m4a", "audio/x-m4b", "audio/webm", "audio/wav", "audio/x-ms-wma",
		},
	}
	path := "/api/items/" + url.PathEscape(id) + "/play"
	if episodeID != "" {
		path += "/" + url.PathEscape(episodeID)
	}
	var s PlaySession
	if err := c.doJSON(ctx, http.MethodPost, path, req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) SyncSession(ctx context.Context, sessionID string, currentTime, timeListened, duration float64) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/sync", map[string]float64{
		"currentTime": currentTime, "timeListened": timeListened, "duration": duration,
	}, nil)
}

func (c *Client) CloseSession(ctx context.Context, sessionID string, currentTime, timeListened, duration float64) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sessionID)+"/close", map[string]float64{
		"currentTime": currentTime, "timeListened": timeListened, "duration": duration,
	}, nil)
}

func (c *Client) SyncLocalSession(ctx context.Context, s LocalSession) error {
	return c.doJSON(ctx, http.MethodPost, "/api/session/local", s, nil)
}

// CoverPath returns the API path of an item's cover, resized server-side.
func CoverPath(id string, width int) string {
	return fmt.Sprintf("/api/items/%s/cover?width=%d&format=jpeg", url.PathEscape(id), width)
}
