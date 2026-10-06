package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSimkl struct {
	mu       sync.Mutex
	refreshs int
	gotToken string
}

func newFakeSimkl(t *testing.T) (*fakeSimkl, *httptest.Server) {
	f := &fakeSimkl{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/oauth2/token":
			r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" || r.Form.Get("client_id") != "cid" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			f.refreshs++
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access-2", "expires_in": 604800, "token_type": "bearer"})
		case "/sync/all-items/":
			f.gotToken = r.Header.Get("Authorization")
			w.Write([]byte(`{
			 "movies":[
			  {"status":"completed","movie":{"title":"Seen It","ids":{"tmdb":101}}},
			  {"status":"dropped","movie":{"title":"Dropped It","ids":{"tmdb":102}}},
			  {"status":"completed","last_watched_at":"2026-07-20T20:00:00Z","movie":{"title":"Cinema","ids":{"tmdb":103}}},
			  {"status":"completed","last_watched_at":"2026-08-01T20:00:00Z","movie":{"title":"Copy Wanted","ids":{"tmdb":107}}},
			  {"status":"plantowatch","movie":{"title":"Want Again","ids":{"tmdb":104}}},
			  {"status":"watching","movie":{"title":"Own It","ids":{"tmdb":105}}},
			  {"status":"plantowatch","movie":{"title":"Not Yet","ids":{"tmdb":106}}}],
			 "shows":[
			  {"status":"watching","show":{"title":"Netflix Show","ids":{"tmdb":201}}},
			  {"status":"completed","show":{"title":"Old Show","ids":{"tmdb":202}}},
			  {"status":"watching","show":{"title":"Sonarr Show","ids":{"tmdb":203}}},
			  {"status":"plantowatch","show":{"title":"Maybe","ids":{"tmdb":204}}}],
			 "anime":[{"status":"hold","show":{"title":"Paused","ids":{"tmdb":301}}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// fakeServices plays Seerr, Sonarr and Radarr on one server.
type fakeServices struct {
	mu         sync.Mutex
	blocklist  map[string]bool // "movie:101"
	posted     []string
	deleted    []string
	exclusions []int
	moviePuts  map[string]map[string]any
}

func newFakeServices(t *testing.T) (*fakeServices, *httptest.Server) {
	f := &fakeServices{blocklist: map[string]bool{"tv:202": true, "movie:104": true}, moviePuts: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/api/v1/blocklist" && r.Method == http.MethodGet:
			var res []map[string]any
			for k := range f.blocklist {
				typ, id, _ := strings.Cut(k, ":")
				var n int
				fmt.Sscan(id, &n)
				res = append(res, map[string]any{"tmdbId": n, "mediaType": typ})
			}
			json.NewEncoder(w).Encode(map[string]any{"pageInfo": map[string]any{"results": len(res)}, "results": res})
		case p == "/api/v1/blocklist" && r.Method == http.MethodPost:
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			k := fmt.Sprintf("%v:%v", b["mediaType"], b["tmdbId"])
			f.posted = append(f.posted, k)
			f.blocklist[k] = true
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(p, "/api/v1/blocklist/") && r.Method == http.MethodDelete:
			k := r.URL.Query().Get("mediaType") + ":" + strings.TrimPrefix(p, "/api/v1/blocklist/")
			f.deleted = append(f.deleted, k)
			delete(f.blocklist, k)
		case strings.HasPrefix(p, "/api/v1/movie/"):
			status := 0
			if p == "/api/v1/movie/105" {
				status = 5 // available in the Jellyfin library
			}
			json.NewEncoder(w).Encode(map[string]any{"title": "T" + strings.TrimPrefix(p, "/api/v1/movie/"), "releaseDate": "2020-01-01",
				"mediaInfo": map[string]any{"status": status}})
		case p == "/api/v3/series":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "Sonarr Show", "tmdbId": 203}})
		case p == "/api/v3/movie" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				// watched 2026-07-20, digital release 2026-09-29: cinema
				{"id": 7, "title": "Cinema", "tmdbId": 103, "monitored": true, "hasFile": false, "digitalRelease": "2026-09-29T00:00:00Z", "physicalRelease": "2026-11-17T00:00:00Z"},
				// watched 2026-08-01, released on disc long before: a copy is wanted
				{"id": 11, "title": "Copy Wanted", "tmdbId": 107, "monitored": true, "hasFile": false, "physicalRelease": "2010-11-24T00:00:00Z"},
				// completed without a watch date, but already released: left alone
				{"id": 10, "title": "Seen It", "tmdbId": 101, "monitored": true, "hasFile": false, "digitalRelease": "2010-07-22T00:00:00Z"},
				{"id": 8, "title": "Own It", "tmdbId": 105, "monitored": true, "hasFile": false},
				{"id": 9, "title": "Not Yet", "tmdbId": 106, "monitored": true, "hasFile": false},
			})
		case strings.HasPrefix(p, "/api/v3/movie/") && r.Method == http.MethodPut:
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			f.moviePuts[p] = b
		case p == "/api/v3/exclusions" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{})
		case p == "/api/v3/exclusions" && r.Method == http.MethodPost:
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			f.exclusions = append(f.exclusions, int(b["tmdbId"].(float64)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func writeToken(t *testing.T, dir string, obtained time.Time) string {
	p := filepath.Join(dir, "simkl.json")
	data, _ := json.Marshal(simklToken{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: 604800, ObtainedAt: obtained.Unix()})
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func simklApp(t *testing.T, obtained time.Time) (*App, *fakeSimkl, *fakeServices, string) {
	fsimkl, ssrv := newFakeSimkl(t)
	old := simklAPI
	simklAPI = ssrv.URL
	t.Cleanup(func() { simklAPI = old })
	fsvc, vsrv := newFakeServices(t)
	a := newTestApp(t)
	tokenFile := writeToken(t, t.TempDir(), obtained)
	a.Simkl = NewSimkl("cid", tokenFile)
	a.Seerr = NewSeerr(vsrv.URL, "k", 1)
	a.Sonarr = NewSonarr(vsrv.URL, "k")
	a.Radarr = NewArr(vsrv.URL, "k")
	return a, fsimkl, fsvc, tokenFile
}

func TestSimklSyncAppliesHouseRules(t *testing.T) {
	a, fsimkl, fsvc, _ := simklApp(t, time.Now())
	a.SimklSync()
	sort.Strings(fsvc.posted)
	if got := fmt.Sprint(fsvc.posted); got != "[movie:102 tv:201 tv:301]" { // 101, 103, 107 are in Radarr
		t.Errorf("blocklisted %s (want completed/dropped movies and watching/hold shows; not tv:202 already there, not tv:203, movie:101 (in Radarr now) or movie:103 managed by Sonarr/Radarr)", got)
	}
	if fmt.Sprint(fsvc.deleted) != "[movie:104]" {
		t.Errorf("unblocked %v, want the movie back on plan to watch", fsvc.deleted)
	}
	if fmt.Sprint(fsvc.exclusions) != "[105]" {
		t.Errorf("exclusions %v, want the wanted movie that's already in the collection", fsvc.exclusions)
	}
	if len(fsvc.moviePuts) != 2 || fsvc.moviePuts["/api/v3/movie/7"]["monitored"] != false || fsvc.moviePuts["/api/v3/movie/8"]["monitored"] != false {
		t.Errorf("radarr puts %v, want Cinema (completed, unreleased) and Own It (in collection) unmonitored; Seen It (completed, released) and Not Yet untouched", fsvc.moviePuts)
	}
	if fsimkl.refreshs != 0 || fsimkl.gotToken != "Bearer access-1" {
		t.Errorf("refreshs %d token %q", fsimkl.refreshs, fsimkl.gotToken)
	}
}

func TestSimklTokenRenewedBeforeExpiry(t *testing.T) {
	a, fsimkl, _, tokenFile := simklApp(t, time.Now().Add(-6*24*time.Hour-time.Hour)) // < 1 day left
	a.SimklSync()
	if fsimkl.refreshs != 1 || fsimkl.gotToken != "Bearer access-2" {
		t.Fatalf("refreshs %d token %q", fsimkl.refreshs, fsimkl.gotToken)
	}
	var tok simklToken
	data, _ := os.ReadFile(tokenFile)
	json.Unmarshal(data, &tok)
	if tok.AccessToken != "access-2" || tok.RefreshToken != "refresh-1" || time.Since(time.Unix(tok.ObtainedAt, 0)) > time.Minute {
		t.Fatalf("saved token %+v (refresh token must be kept when Simkl doesn't send a new one)", tok)
	}
	if st, _ := os.Stat(tokenFile); st.Mode().Perm() != 0600 {
		t.Fatalf("token file mode %v", st.Mode().Perm())
	}
	a.SimklSync() // fresh now: no second refresh
	if fsimkl.refreshs != 1 {
		t.Fatalf("refreshed again: %d", fsimkl.refreshs)
	}
}

func TestSimklSyncDryRun(t *testing.T) {
	a, _, fsvc, _ := simklApp(t, time.Now())
	a.Config.DryRun = true
	a.SimklSync()
	if len(fsvc.posted)+len(fsvc.deleted)+len(fsvc.exclusions)+len(fsvc.moviePuts) != 0 {
		t.Fatalf("dry run changed things: %v %v %v %v", fsvc.posted, fsvc.deleted, fsvc.exclusions, fsvc.moviePuts)
	}
}

func TestSimklMissingTokenFile(t *testing.T) {
	a, fsimkl, fsvc, tokenFile := simklApp(t, time.Now())
	os.Remove(tokenFile)
	a.SimklSync()
	if fsimkl.gotToken != "" || len(fsvc.posted) != 0 {
		t.Fatal("sync must stop without a token")
	}
}

func TestSeenInCinema(t *testing.T) {
	watched := map[int]time.Time{1: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), 2: {}}
	cases := []struct {
		tmdb  int
		movie map[string]any
		want  bool
	}{
		{1, map[string]any{"digitalRelease": "2026-09-29T00:00:00Z"}, true},                                             // before digital
		{1, map[string]any{"digitalRelease": "2026-12-01T00:00:00Z", "physicalRelease": "2026-07-01T00:00:00Z"}, false}, // disc came first
		{1, map[string]any{}, true}, // no home release yet
		{2, map[string]any{"digitalRelease": "2026-09-29T00:00:00Z"}, false}, // no watch date, released
		{2, map[string]any{}, true},  // no watch date, unreleased
		{3, map[string]any{}, false}, // not completed
	}
	for i, c := range cases {
		if got := seenInCinema(watched, c.tmdb, c.movie); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestSimklCollectionRuleUsesPlex(t *testing.T) {
	a, _, fsvc, _ := simklApp(t, time.Now())
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			fmt.Fprint(w, `{"MediaContainer":{"Directory":[{"key":"4","type":"movie","Location":[{"path":"/volume1/Video/Movies"}]}]}}`)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"Guid":[{"id":"tmdb://106"}]}]}}`)
	}))
	defer plex.Close()
	a.Plex = NewPlex(plex.URL, "/volume1/Video")
	a.SimklSync()
	if fsvc.moviePuts["/api/v3/movie/9"]["monitored"] != false {
		t.Errorf("Not Yet (tmdb 106) is in Plex's collection and should be unmonitored; puts %v", fsvc.moviePuts)
	}
}

func TestSimklRulesActOncePerMovie(t *testing.T) {
	a, _, fsvc, _ := simklApp(t, time.Now())
	a.Config.SimklRulesFile = filepath.Join(t.TempDir(), "rules.json")
	a.SimklSync()
	if len(fsvc.moviePuts) != 2 || len(fsvc.exclusions) != 1 {
		t.Fatalf("first run: puts %v exclusions %v", fsvc.moviePuts, fsvc.exclusions)
	}
	// The owner monitors both movies again and deletes the exclusion (the fake keeps reporting them monitored and
	// unexcluded): the rules must not undo that.
	fsvc.moviePuts, fsvc.exclusions = map[string]map[string]any{}, nil
	a.SimklSync()
	if len(fsvc.moviePuts) != 0 || len(fsvc.exclusions) != 0 {
		t.Errorf("second run touched handled movies again: puts %v exclusions %v", fsvc.moviePuts, fsvc.exclusions)
	}
}

func TestRuleMemorySeedsFromRadarr(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rules.json")
	movies := []map[string]any{
		{"tmdbId": float64(1), "monitored": false, "hasFile": false}, // unmonitored earlier (e.g. Spider-Man)
		{"tmdbId": float64(2), "monitored": true, "hasFile": false},
		{"tmdbId": float64(3), "monitored": false, "hasFile": true},
	}
	m := loadRuleMemory(p, movies, map[int]bool{9: true})
	if len(m.Unmonitored) != 1 || m.Unmonitored[1] == "" || m.Excluded[9] == "" {
		t.Fatalf("seed %+v", m)
	}
	if again := loadRuleMemory(p, nil, nil); again.Unmonitored[1] == "" || again.Excluded[9] == "" {
		t.Fatalf("not persisted: %+v", again)
	}
}
