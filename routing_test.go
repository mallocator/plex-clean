package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeArr serves tags and items for one kind ("series" or "movie") and records PUTs.
type fakeArr struct {
	mu    sync.Mutex
	kind  string
	items []map[string]any
	puts  map[string]map[string]any // path?query -> body
}

func newFakeArr(t *testing.T, kind string, items []map[string]any) (*fakeArr, *httptest.Server) {
	f := &fakeArr{kind: kind, items: items, puts: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/tag":
			json.NewEncoder(w).Encode([]SonarrTag{{ID: 1, Label: "delete-after-watch"}, {ID: 2, Label: "2-daniela"},
				{ID: 3, Label: "1-mallox"}, {ID: 4, Label: "5 - daniela"}, {ID: 5, Label: "7-guest"}})
		case r.URL.Path == "/api/v3/"+kind && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.items)
		case strings.HasPrefix(r.URL.Path, "/api/v3/"+kind+"/") && r.Method == http.MethodPut:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.puts[r.URL.Path+"?"+r.URL.RawQuery] = body
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func routeApp(t *testing.T) *App {
	a := newTestApp(t)
	a.Config.UserFolders = map[string]string{"daniela": "/downloads/daniela", "mallox": "/downloads/ravi"}
	a.Config.TVSubdir, a.Config.MovieSubdir = "tv", "movies"
	return a
}

func TestRouteMovesSeriesToRequesterFolder(t *testing.T) {
	a := routeApp(t)
	fs, srv := newFakeArr(t, "series", []map[string]any{
		{"id": 10, "title": "Bridgerton", "path": "/downloads/ravi/tv/Bridgerton", "tags": []int{1, 2}},  // Daniela: move
		{"id": 11, "title": "Old Tag", "path": "/downloads/ravi/tv/Old Tag", "tags": []int{4}},           // "5 - daniela": move
		{"id": 12, "title": "Severance", "path": "/downloads/ravi/tv/Severance", "tags": []int{3}},       // mallox, already right
		{"id": 13, "title": "The Simpsons", "path": "/downloads/ravi/tv/The Simpsons", "tags": []int{1}}, // no user tag
		{"id": 14, "title": "Guest Show", "path": "/downloads/ravi/tv/Guest Show", "tags": []int{5}},     // unmapped user
		{"id": 15, "title": "Grey's", "path": "/downloads/daniela/tv/Grey's", "tags": []int{2}},          // already right
		{"id": 16, "title": "Mine", "path": "/downloads/daniela/tv/Mine", "tags": []int{3}},              // mallox: move back
	})
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Route()
	want := map[string]string{
		"/api/v3/series/10?moveFiles=true": "/downloads/daniela/tv/Bridgerton",
		"/api/v3/series/11?moveFiles=true": "/downloads/daniela/tv/Old Tag",
		"/api/v3/series/16?moveFiles=true": "/downloads/ravi/tv/Mine",
	}
	if len(fs.puts) != len(want) {
		t.Fatalf("puts %v", fs.puts)
	}
	for k, path := range want {
		body, ok := fs.puts[k]
		if !ok || body["path"] != path || body["rootFolderPath"] != path[:strings.LastIndex(path, "/")] {
			t.Errorf("%s: got %v", k, body)
		}
		if body["title"] == nil {
			t.Errorf("%s: PUT must send the full object, got %v", k, body)
		}
	}
}

func TestRouteMoviesUseMovieSubdir(t *testing.T) {
	a := routeApp(t)
	fr, srv := newFakeArr(t, "movie", []map[string]any{
		{"id": 3, "title": "Koln 75", "path": "/downloads/ravi/movies/Koln 75 (2025)", "tags": []int{2}},
	})
	a.Radarr = NewArr(srv.URL, "k")
	a.Route()
	if fr.puts["/api/v3/movie/3?moveFiles=true"]["path"] != "/downloads/daniela/movies/Koln 75 (2025)" {
		t.Fatalf("puts %v", fr.puts)
	}
}

func TestRouteDryRunAndDisabled(t *testing.T) {
	a := routeApp(t)
	a.Config.DryRun = true
	fs, srv := newFakeArr(t, "series", []map[string]any{{"id": 10, "title": "X", "path": "/downloads/ravi/tv/X", "tags": []int{2}}})
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Route()
	a.Config.DryRun = false
	a.Config.UserFolders = nil // no mapping: routing off
	a.Route()
	if len(fs.puts) != 0 {
		t.Fatalf("puts %v", fs.puts)
	}
}

func TestSonarrWebhookTriggersRoute(t *testing.T) {
	a := routeApp(t)
	fs, srv := newFakeArr(t, "series", []map[string]any{{"id": 10, "title": "X", "path": "/downloads/ravi/tv/X", "tags": []int{2}}})
	a.Sonarr = NewSonarr(srv.URL, "k")
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sonarr", strings.NewReader(`{"eventType":"SeriesAdd"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	for i := 0; i < 50; i++ {
		fs.mu.Lock()
		n := len(fs.puts)
		fs.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("webhook did not route the new series")
}

func TestParseMap(t *testing.T) {
	m := parseMap(" daniela=/downloads/daniela/ , mallox=/downloads/ravi,bad")
	if len(m) != 2 || m["daniela"] != "/downloads/daniela" || m["mallox"] != "/downloads/ravi" {
		t.Fatalf("%v", m)
	}
}
