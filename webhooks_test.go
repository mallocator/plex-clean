package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	q, err := LoadQueue(filepath.Join(dir, "state", "pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	return &App{
		Config: Config{OutputDir: filepath.Join(dir, "purge"), GracePeriod: 24 * time.Hour,
			DeleteTag: "delete-after-watch", ArchiveTag: "archive", ArchiveDir: filepath.Join(dir, "archive")},
		Queue: q,
		Now:   func() time.Time { return now },
	}
}

func plexRequest(t *testing.T, payload string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("payload", payload); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/plex", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

const plexScrobble = `{"event":"media.scrobble","Account":{"title":"mallox"},
 "Metadata":{"type":"episode","title":"Treehouse of Horror","grandparentTitle":"The Simpsons","parentIndex":38,"index":2,
 "Guid":[{"id":"tvdb://123"}]}}`

func TestPlexScrobbleQueuesEpisodeAndWritesMarker(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, plexRequest(t, plexScrobble))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	items := a.Queue.Items()
	if len(items) != 1 || items[0].Series != "The Simpsons" || items[0].Season != 38 || items[0].Episode != 2 || items[0].Source != "plex" {
		t.Fatalf("queue = %+v", items)
	}
	marker := filepath.Join(a.Config.OutputDir, "The Simpsons - Treehouse of Horror - S38E2.json")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if !strings.Contains(string(data), `"watched_status": 1`) || !strings.Contains(string(data), `"parent_media_index": 38`) {
		t.Errorf("marker content %s", data)
	}
}

func TestPlexIgnoresOtherEvents(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, plexRequest(t, strings.Replace(plexScrobble, "media.scrobble", "media.stop", 1)))
	if rec.Code != http.StatusOK || len(a.Queue.Items()) != 0 {
		t.Fatalf("status %d, queue %+v", rec.Code, a.Queue.Items())
	}
}

func TestPlexMovieWritesMarkerOnly(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, plexRequest(t, `{"event":"media.scrobble","Metadata":{"type":"movie","title":"Koln 75"}}`))
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("movies must not be queued: %+v", a.Queue.Items())
	}
	if _, err := os.Stat(filepath.Join(a.Config.OutputDir, "Koln 75.json")); err != nil {
		t.Fatalf("marker: %v", err)
	}
}

func TestPlexBadPayload(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, plexRequest(t, "{not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}

func jellyfinRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/jellyfin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestJellyfinCompletedEpisodeIsQueued(t *testing.T) {
	a := newTestApp(t)
	for _, body := range []string{
		// nested MediaStatus and top-level flag (both template styles)
		`{"NotificationType":"PlaybackStop","ItemType":"Episode","Name":"Ep","SeriesName":"Futurama","SeasonNumber":11,"EpisodeNumber":10,"MediaStatus":{"PlayedToCompletion":true}}`,
		`{"NotificationType":"PlaybackStop","ItemType":"Episode","Name":"Ep","SeriesName":"Futurama","SeasonNumber":11,"EpisodeNumber":9,"PlayedToCompletion":true}`,
	} {
		rec := httptest.NewRecorder()
		a.Routes().ServeHTTP(rec, jellyfinRequest(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if n := len(a.Queue.Items()); n != 2 {
		t.Fatalf("queued %d, want 2", n)
	}
}

func TestJellyfinIncompleteIgnored(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, jellyfinRequest(`{"NotificationType":"PlaybackStop","ItemType":"Episode","SeriesName":"Futurama","SeasonNumber":11,"EpisodeNumber":10,"PlayedToCompletion":false}`))
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("queue %+v", a.Queue.Items())
	}
}

func TestRootRouteDetectsSource(t *testing.T) {
	a := newTestApp(t)
	req := plexRequest(t, plexScrobble)
	req.URL.Path = "/"
	a.Routes().ServeHTTP(httptest.NewRecorder(), req)
	req = jellyfinRequest(`{"NotificationType":"PlaybackStop","ItemType":"Episode","SeriesName":"Futurama","SeasonNumber":1,"EpisodeNumber":1,"PlayedToCompletion":true}`)
	req.URL.Path = "/"
	a.Routes().ServeHTTP(httptest.NewRecorder(), req)
	if n := len(a.Queue.Items()); n != 2 {
		t.Fatalf("queued %d, want 2", n)
	}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown content type: status %d", rec.Code)
	}
}

func TestWatchingTwiceKeepsFirstTime(t *testing.T) {
	a := newTestApp(t)
	e := WatchEvent{Source: "plex", Type: "episode", Series: "The Simpsons", Season: 38, Episode: 2}
	first := a.now()
	a.Queue.Add(e, first)
	e.Source = "jellyfin"
	if a.Queue.Add(e, first.Add(5*time.Hour)) {
		t.Fatal("second add should report already queued")
	}
	if got := a.Queue.Items()[0]; !got.WatchedAt.Equal(first) || got.Source != "plex" {
		t.Fatalf("item %+v", got)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"The Simpsons":           "simpsons",
		"Simpsons":               "simpsons",
		"Dark Matter (2024)":     "darkmatter",
		"Hell's Kitchen (US)":    "hellskitchenus",
		"Star Trek: Lower Decks": "startreklowerdecks",
	}
	for in, want := range cases {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooseEpisodeReportedAsMovieIsQueued(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, jellyfinRequest(`{"NotificationType":"PlaybackStop","ItemType":"Movie","Name":"The Great British Bake Off S17E01 Cake Week","PlayedToCompletion":true}`))
	a.Routes().ServeHTTP(httptest.NewRecorder(), plexRequest(t, `{"event":"media.scrobble","Metadata":{"type":"movie","title":"Bad.Monkey.2024.S01E09.1080p"}}`))
	a.Routes().ServeHTTP(httptest.NewRecorder(), jellyfinRequest(`{"NotificationType":"PlaybackStop","ItemType":"Movie","Name":"Snatch","PlayedToCompletion":true}`))
	items := a.Queue.Items()
	if len(items) != 2 {
		t.Fatalf("queued %+v, want the two episodes but not the movie", items)
	}
	got := map[string]bool{}
	for _, it := range items {
		got[fmt.Sprintf("%s S%02dE%02d", it.Series, it.Season, it.Episode)] = true
	}
	if !got["The Great British Bake Off S17E01"] || !got["Bad Monkey S01E09"] {
		t.Errorf("queued %v", got)
	}
}
