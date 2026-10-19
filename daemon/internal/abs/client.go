// Package abs is a small Audiobookshelf API client covering what a listening
// client needs: auth (JWT access/refresh and legacy tokens), browsing,
// playback sessions, progress, and file streaming.
package abs

import (
	"bytes"
	"context"
	"crypto/tls"
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

// LibraryItems fetches one page. sort is an Audiobookshelf sort key such as
// "media.metadata.title" or "addedAt".
func (c *Client) LibraryItems(ctx context.Context, libraryID string, page, limit int, sort string, desc bool) (ItemsPage, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("page", strconv.Itoa(page))
	q.Set("minified", "1")
	q.Set("include", "progress")
	if sort != "" {
		q.Set("sort", sort)
	}
	if desc {
		q.Set("desc", "1")
	}
	var p ItemsPage
	err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/items?"+q.Encode(), nil, &p)
	return p, err
}

func (c *Client) Search(ctx context.Context, libraryID, query string) ([]LibraryItem, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", "50")
	var r searchResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/libraries/"+url.PathEscape(libraryID)+"/search?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	items := make([]LibraryItem, 0, len(r.Book))
	for _, b := range r.Book {
		items = append(items, b.LibraryItem)
	}
	return items, nil
}

func (c *Client) ItemsInProgress(ctx context.Context) ([]LibraryItem, error) {
	var r itemsInProgressResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/me/items-in-progress", nil, &r)
	return r.LibraryItems, err
}

func (c *Client) Item(ctx context.Context, id string) (*LibraryItem, error) {
	var it LibraryItem
	err := c.doJSON(ctx, http.MethodGet, "/api/items/"+url.PathEscape(id)+"?expanded=1&include=progress", nil, &it)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (c *Client) UpdateProgress(ctx context.Context, id string, patch map[string]any) error {
	return c.doJSON(ctx, http.MethodPatch, "/api/me/progress/"+url.PathEscape(id), patch, nil)
}

func (c *Client) StartPlay(ctx context.Context, id string) (*PlaySession, error) {
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
	var s PlaySession
	if err := c.doJSON(ctx, http.MethodPost, "/api/items/"+url.PathEscape(id)+"/play", req, &s); err != nil {
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
