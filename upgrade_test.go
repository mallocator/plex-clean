package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func rel(title string, score int, approved bool, res int, rejections ...string) arrRelease {
	r := arrRelease{GUID: title, IndexerID: 1, Title: title, Size: int64(score+1000) << 20, Approved: approved,
		Rejections: rejections, CustomFormatScore: score}
	r.Quality.Quality.Resolution = res
	return r
}

func TestFallbackRelease(t *testing.T) {
	floor := "560.0 MB is smaller than minimum allowed 780.0 MB (for 65min)"
	cases := []struct {
		name string
		rs   []arrRelease
		want string
	}{
		{"best under the floor", []arrRelease{
			rel("MeGusta", -490, false, 1080, floor),
			rel("CAKES", 200, false, 1080, floor),
			rel("720p WEB-DL", 370, false, 720, "WEBDL-720p is not wanted in profile"),
			rel("blocklisted", 300, false, 1080, floor, "Release is blocklisted"),
		}, "CAKES"},
		{"720p when nothing 1080p", []arrRelease{
			rel("480p", 0, false, 480, "SDTV is not wanted in profile"),
			rel("720p small", 50, false, 720, "HDTV-720p is not wanted in profile", floor),
			rel("720p WEB-DL", 370, false, 720, "WEBDL-720p is not wanted in profile"),
			rel("other show", 500, false, 720, "Unknown Series"),
		}, "720p WEB-DL"},
		{"nothing usable", []arrRelease{
			rel("480p", 0, false, 480, "SDTV is not wanted in profile"),
			rel("AV1", 0, false, 1080, "Custom Formats AV1 have score -10000 below minimum"),
		}, ""},
	}
	for _, c := range cases {
		got, _, ok := fallbackRelease(c.rs)
		if got.Title != c.want || ok != (c.want != "") {
			t.Errorf("%s: got %q (%v), want %q", c.name, got.Title, ok, c.want)
		}
	}
}

func TestSearchEvery(t *testing.T) {
	w := 14 * 24 * time.Hour
	for _, c := range []struct {
		age, want time.Duration
	}{{time.Hour, 30 * time.Minute}, {20 * time.Hour, time.Hour}, {5 * 24 * time.Hour, 6 * time.Hour}, {60 * 24 * time.Hour, 24 * time.Hour}} {
		if got := searchEvery(c.age, w); got != c.want {
			t.Errorf("age %s: every %s, want %s", c.age, got, c.want)
		}
	}
}

type fakeUpgradeArr struct {
	mu       sync.Mutex
	searched []string
	grabbed  []string
	queued   []map[string]any // grabs show up in the queue, as in Sonarr
}

func TestUpgradeSearch(t *testing.T) {
	now := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	file := func(path string, score int) map[string]any {
		return map[string]any{"path": path, "customFormatScore": score, "qualityCutoffNotMet": false}
	}
	ep := func(id, e int, aired string, f map[string]any) map[string]any {
		return map[string]any{"id": id, "seasonNumber": 2, "episodeNumber": e, "airDateUtc": aired, "monitored": true,
			"hasFile": f != nil, "episodeFile": f, "series": map[string]any{"title": "Dark Matter (2024)", "qualityProfileId": 4}}
	}
	calendar := []map[string]any{
		ep(1, 8, at(20*time.Hour), file("/downloads/ravi/tv/Dark Matter/S02E08.mkv", -490)),  // re-encode: upgrade
		ep(2, 7, at(9*24*time.Hour), file("/downloads/ravi/tv/Dark Matter/S02E07.mkv", 300)), // good: done
		ep(3, 6, at(2*time.Hour), nil), // missing, new: no fallback yet
		ep(4, 5, at(30*time.Hour), file("/downloads/ravi/tv/Dark Matter/S02E05.mkv", -490)), // watched
		ep(5, 4, at(40*time.Hour), file("/downloads/ravi/tv/Dark Matter/S02E04.mkv", -490)), // downloading
		ep(6, 9, now.Add(time.Hour).Format(time.RFC3339), nil),                              // not aired
	}
	missing := []map[string]any{ep(7, 1, at(400*24*time.Hour), nil)} // old and missing: fallback after 2 days
	f := &fakeUpgradeArr{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		floor := "500 MB is smaller than minimum allowed 780.0 MB (for 65min)"
		switch {
		case r.URL.Path == "/api/v3/qualityprofile":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 4, "cutoffFormatScore": 200}})
		case r.URL.Path == "/api/v3/calendar":
			json.NewEncoder(w).Encode(calendar)
		case r.URL.Path == "/api/v3/wanted/missing":
			json.NewEncoder(w).Encode(map[string]any{"records": missing})
		case r.URL.Path == "/api/v3/queue":
			json.NewEncoder(w).Encode(map[string]any{"records": append([]map[string]any{{"id": 9, "episodeId": 5, "title": "S02E04"}}, f.queued...)})
		case r.URL.Path == "/api/v3/release" && r.Method == http.MethodGet:
			id := r.URL.Query().Get("episodeId")
			f.searched = append(f.searched, id)
			var rs []arrRelease
			switch id {
			case "1":
				rs = []arrRelease{rel("TBK", 280, true, 1080), rel("POOTLED", -20, true, 1080)}
			case "3", "7":
				rs = []arrRelease{rel("small-"+id, -490, false, 1080, floor)}
			}
			json.NewEncoder(w).Encode(rs)
		case r.URL.Path == "/api/v3/release" && r.Method == http.MethodPost:
			var body struct {
				GUID string `json:"guid"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.grabbed = append(f.grabbed, body.GUID)
			if body.GUID == "TBK" {
				f.queued = append(f.queued, map[string]any{"id": 10, "episodeId": 1, "title": "S02E08"})
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := newTestApp(t)
	a.Now = func() time.Time { return now }
	a.Config.UpgradeWindow, a.Config.UpgradeFallback, a.Config.UpgradeMaxSearches = 14*24*time.Hour, 48*time.Hour, 6
	a.Config.UpgradeStateFile = filepath.Join(t.TempDir(), "upgrades.json")
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Queue.Add(WatchEvent{Source: "jellyfin", Type: "episode", Series: "Dark Matter (2024)", Season: 2, Episode: 5}, now)

	a.UpgradeSearch()
	sort.Strings(f.searched)
	if strings.Join(f.searched, ",") != "1,3,7" || strings.Join(f.grabbed, ",") != "TBK" {
		t.Fatalf("first run: searched %v grabbed %v, want 1,3,7 searched and TBK grabbed", f.searched, f.grabbed)
	}

	// 40 minutes later: only the new missing episode (every 30 min) is due again
	f.searched, f.grabbed = nil, nil
	now = now.Add(40 * time.Minute)
	a.UpgradeSearch()
	if strings.Join(f.searched, ",") != "3" || len(f.grabbed) != 0 {
		t.Fatalf("second run: searched %v grabbed %v, want only 3 and no grab", f.searched, f.grabbed)
	}

	// two days later the old missing episode (tracked since the first run) falls back to the release under the
	// floor; episode 3 aired 2 h before the first run, so it's past 48 h too. Episode 1 is downloading, episode 6 has
	// aired meanwhile (searched, nothing found). State survives a restart.
	f.searched, f.grabbed = nil, nil
	now = now.Add(48 * time.Hour)
	b := newTestApp(t)
	b.Now, b.Config, b.Sonarr = a.Now, a.Config, a.Sonarr
	b.Queue = a.Queue
	b.UpgradeSearch()
	sort.Strings(f.grabbed)
	sort.Strings(f.searched)
	if strings.Join(f.searched, ",") != "3,6,7" || strings.Join(f.grabbed, ",") != "small-3,small-7" {
		t.Fatalf("fallback run: searched %v grabbed %v, want small-3 and small-7", f.searched, f.grabbed)
	}

	// the per-run limit
	f.searched, f.grabbed = nil, nil
	now = now.Add(48 * time.Hour)
	b.Config.UpgradeMaxSearches = 1
	b.UpgradeSearch()
	if len(f.searched) != 1 {
		t.Fatalf("limit: searched %v, want 1", f.searched)
	}
}

func TestUpgradeSkipsEpisodesPlayedInJellyfin(t *testing.T) {
	now := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			json.NewEncoder(w).Encode([]map[string]any{{"Id": "u1"}})
		case "/Users/u1/Items":
			json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
				{"SeriesName": "The Simpsons", "ParentIndexNumber": 38, "IndexNumber": 2}}})
		default:
			json.NewEncoder(w).Encode([]any{}) // sessions
		}
	}))
	defer jf.Close()
	var searched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep := func(id, e int) map[string]any {
			return map[string]any{"id": id, "seasonNumber": 38, "episodeNumber": e, "airDateUtc": now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
				"monitored": true, "hasFile": false, "series": map[string]any{"title": "The Simpsons", "qualityProfileId": 4}}
		}
		switch r.URL.Path {
		case "/api/v3/qualityprofile":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 4, "cutoffFormatScore": 200}})
		case "/api/v3/calendar":
			json.NewEncoder(w).Encode([]any{})
		case "/api/v3/wanted/missing":
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{ep(1, 2), ep(2, 3)}})
		case "/api/v3/queue":
			json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
		case "/api/v3/release":
			searched = append(searched, r.URL.Query().Get("episodeId"))
			json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer srv.Close()
	a := newTestApp(t)
	a.Now = func() time.Time { return now }
	a.Config.UpgradeWindow, a.Config.UpgradeFallback, a.Config.UpgradeMaxSearches = 14*24*time.Hour, 48*time.Hour, 6
	a.Config.UpgradeStateFile = filepath.Join(t.TempDir(), "upgrades.json")
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.Jellyfin = NewJellyfin(jf.URL, "k")
	a.UpgradeSearch()
	if strings.Join(searched, ",") != "2" {
		t.Errorf("searched %v, want only S38E03 (S38E02 was played: watched and deleted, not fetched again)", searched)
	}
}
