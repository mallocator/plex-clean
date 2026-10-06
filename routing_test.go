package main

import (
	"encoding/json"
	"fmt"
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

	created  []string // tag labels created via POST
	noMallox bool     // tag list lacks "1-mallox"
}

func newFakeArr(t *testing.T, kind string, items []map[string]any) (*fakeArr, *httptest.Server) {
	f := &fakeArr{kind: kind, items: items, puts: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/tag" && r.Method == http.MethodPost:
			var t SonarrTag
			json.NewDecoder(r.Body).Decode(&t)
			t.ID = 100 + len(f.created)
			f.created = append(f.created, t.Label)
			json.NewEncoder(w).Encode(t)
		case r.URL.Path == "/api/v3/tag":
			tags := []SonarrTag{{ID: 1, Label: "delete-after-watch"}, {ID: 2, Label: "2-daniela"},
				{ID: 4, Label: "5 - daniela"}, {ID: 5, Label: "7-guest"}}
			if !f.noMallox {
				tags = append(tags, SonarrTag{ID: 3, Label: "1-mallox"})
			}
			json.NewEncoder(w).Encode(tags)
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

func TestRouteTagsUntaggedItemsByFolder(t *testing.T) {
	a := routeApp(t)
	a.Config.UserFolders = map[string]string{"2-daniela": "/downloads/daniela", "1-mallox": "/downloads/ravi"}
	fs, srv := newFakeArr(t, "series", []map[string]any{
		{"id": 20, "title": "The Simpsons", "path": "/downloads/ravi/tv/The Simpsons", "tags": []int{1}}, // -> 1-mallox
		{"id": 21, "title": "Her Show", "path": "/downloads/daniela/tv/Her Show", "tags": []int{}},       // -> 2-daniela
		{"id": 22, "title": "Elsewhere", "path": "/downloads/sonarr/Elsewhere", "tags": []int{}},         // not a user folder
		{"id": 23, "title": "Tagged", "path": "/downloads/daniela/tv/Tagged", "tags": []int{2}},          // already tagged
	})
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Route()
	if len(fs.puts) != 2 {
		t.Fatalf("puts %v", fs.puts)
	}
	if got := fs.puts["/api/v3/series/20?"]; got == nil || fmt.Sprint(got["tags"]) != "[1 3]" || got["path"] != "/downloads/ravi/tv/The Simpsons" {
		t.Errorf("simpsons: %v", got)
	}
	if got := fs.puts["/api/v3/series/21?"]; got == nil || fmt.Sprint(got["tags"]) != "[2]" {
		t.Errorf("her show: %v", got)
	}
	if len(fs.created) != 0 {
		t.Errorf("existing tags must be reused, created %v", fs.created)
	}
}

func TestRouteCreatesMissingUserTag(t *testing.T) {
	a := routeApp(t)
	a.Config.UserFolders = map[string]string{"2-daniela": "/downloads/daniela", "1-mallox": "/downloads/ravi"}
	fr, srv := newFakeArr(t, "movie", []map[string]any{
		{"id": 5, "title": "Lee", "path": "/downloads/ravi/movies/Lee (2024)", "tags": []int{}},
	})
	fr.noMallox = true
	a.Radarr = NewArr(srv.URL, "k")
	a.Route()
	if fmt.Sprint(fr.created) != "[1-mallox]" || fmt.Sprint(fr.puts["/api/v3/movie/5?"]["tags"]) != "[100]" {
		t.Fatalf("created %v puts %v", fr.created, fr.puts)
	}
}

func TestRouteTagDryRun(t *testing.T) {
	a := routeApp(t)
	a.Config.DryRun = true
	a.Config.UserFolders = map[string]string{"2-daniela": "/downloads/daniela"}
	fs, srv := newFakeArr(t, "series", []map[string]any{{"id": 21, "title": "Her Show", "path": "/downloads/daniela/tv/Her Show", "tags": []int{}}})
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Route()
	if len(fs.puts) != 0 || len(fs.created) != 0 {
		t.Fatalf("puts %v created %v", fs.puts, fs.created)
	}
}

func TestRouteWithSeerrStyleKeysStillMoves(t *testing.T) {
	a := routeApp(t)
	a.Config.UserFolders = map[string]string{"2-daniela": "/downloads/daniela", "1-mallox": "/downloads/ravi"}
	fs, srv := newFakeArr(t, "series", []map[string]any{{"id": 10, "title": "X", "path": "/downloads/ravi/tv/X", "tags": []int{4}}}) // "5 - daniela"
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Route()
	if fs.puts["/api/v3/series/10?moveFiles=true"]["path"] != "/downloads/daniela/tv/X" {
		t.Fatalf("puts %v", fs.puts)
	}
}
