package abs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"192.168.1.5:13378":          "http://192.168.1.5:13378",
		" https://abs.example.com/ ": "https://abs.example.com",
		"http://host/abs/":           "http://host/abs",
		"https://h.example?x=1":      "https://h.example",
	}
	for in, want := range cases {
		got, err := NormalizeServer(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "http://"} {
		if _, err := NormalizeServer(bad); err == nil {
			t.Errorf("NormalizeServer(%q) should fail", bad)
		}
	}
}

// A 401 must trigger one refresh with the refresh token, then a retry.
func TestRefreshOn401(t *testing.T) {
	refreshed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			if r.Header.Get("x-refresh-token") != "refresh-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			refreshed = true
			_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"accessToken": "access-2", "refreshToken": "refresh-2"}})
		case "/api/libraries":
			if r.Header.Get("Authorization") != "Bearer access-2" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"libraries": []Library{{ID: "lib1", Name: "Books", MediaType: "book"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var saved Credentials
	c := NewClient(DeviceInfo{}, false, func(cr Credentials) { saved = cr })
	c.SetCredentials(Credentials{Server: srv.URL, AccessToken: "access-1", RefreshToken: "refresh-1"})
	libs, err := c.Libraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || len(libs) != 1 || libs[0].ID != "lib1" {
		t.Fatalf("refreshed=%v libs=%v", refreshed, libs)
	}
	if saved.AccessToken != "access-2" || saved.RefreshToken != "refresh-2" {
		t.Fatalf("new tokens not persisted: %+v", saved)
	}
}
