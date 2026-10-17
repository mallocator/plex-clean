package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMinutesAndBitrate(t *testing.T) {
	for rt, want := range map[string]float64{"1:04:32": 64.53, "22:17": 22.28, "": 0, "x": 0} {
		if got := minutes(rt); math.Abs(got-want) > 0.01 {
			t.Errorf("minutes(%q) = %.2f, want %.2f", rt, got, want)
		}
	}
	f := importFile{Size: 560 << 20}
	f.MediaInfo.RunTime = "1:04:32"
	if got := f.mbPerMin(); math.Abs(got-8.68) > 0.01 { // Dark Matter S02E08, MeGusta
		t.Errorf("mbPerMin = %.2f", got)
	}
}

func TestMislabeled(t *testing.T) {
	for _, c := range []struct {
		res  string
		q    int
		want bool
	}{
		{"1280x720", 1080, true},
		{"1920x800", 1080, false},  // 2.40:1 film
		{"1440x1080", 1080, false}, // 4:3
		{"1920x1080", 1080, false},
		{"1920x1080", 2160, true},
		{"3840x1600", 2160, false},
		{"", 1080, false},
	} {
		f := importFile{Resolution: c.q}
		f.MediaInfo.Resolution = c.res
		if got := f.mislabeled(); got != c.want {
			t.Errorf("%s as %dp: mislabeled %v, want %v", c.res, c.q, got, c.want)
		}
	}
}

func TestCheckImportsAndStarvedGroups(t *testing.T) {
	now := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)
	added := now.Add(-24 * time.Hour).Format(time.RFC3339)
	epFile := func(id int, group string, size int64, res string, q int) map[string]any {
		return map[string]any{"id": id, "path": "/downloads/x.mkv", "size": size, "dateAdded": added, "releaseGroup": group,
			"quality":   map[string]any{"quality": map[string]any{"resolution": q}},
			"mediaInfo": map[string]any{"runTime": "1:00:00", "resolution": res}}
	}
	var mu sync.Mutex
	var failed []string
	calendar := []map[string]any{}
	for i := 1; i <= 3; i++ { // three starved files from one group
		calendar = append(calendar, map[string]any{"id": i, "seasonNumber": 1, "episodeNumber": i,
			"series": map[string]any{"title": "Show"}, "episodeFile": epFile(100+i, "TinyRip", 400<<20, "1920x1080", 1080)})
	}
	calendar = append(calendar, map[string]any{"id": 4, "seasonNumber": 1, "episodeNumber": 4, "series": map[string]any{"title": "Show"},
		"episodeFile": epFile(104, "Fake", 2000<<20, "1280x720", 1080)}) // a "1080p" that is 720p
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/calendar":
			json.NewEncoder(w).Encode(calendar)
		case r.URL.Path == "/api/v3/history": // Sonarr: paged, all event types
			if r.URL.Query().Get("episodeId") != "4" {
				t.Errorf("history query %s", r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"id": 7, "eventType": "grabbed", "date": "2026-10-16T10:00:00Z"},
				{"id": 9, "eventType": "grabbed", "date": "2026-10-16T12:00:00Z"},
				{"id": 11, "eventType": "downloadFolderImported", "date": "2026-10-16T13:00:00Z"}}})
		case strings.HasPrefix(r.URL.Path, "/api/v3/history/failed/") && r.Method == http.MethodPost:
			failed = append(failed, strings.TrimPrefix(r.URL.Path, "/api/v3/history/failed/"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := newTestApp(t)
	a.Now = func() time.Time { return now }
	a.Config.UpgradeWindow, a.Config.ImportMinMBPerMin = 14*24*time.Hour, 12
	a.Config.GroupStatsFile = filepath.Join(t.TempDir(), "groups.json")
	a.Sonarr = NewSonarr(srv.URL, "k")
	a.CheckImports()
	a.CheckImports() // each file counts once, each mislabeled file is flagged once
	if strings.Join(failed, ",") != "9" {
		t.Errorf("failed grabs %v, want the latest grab (9) of the mislabeled file, once", failed)
	}
	g := a.groupStats()
	if n := len(g.Groups["tinyrip"]); n != 3 {
		t.Errorf("TinyRip samples %d, want 3", n)
	}
	if !g.starved("TinyRip", 12) || g.starved("Fake", 12) || g.starved("Unknown", 12) {
		t.Errorf("starved: TinyRip %v Fake %v", g.starved("TinyRip", 12), g.starved("Fake", 12))
	}
	// a restart keeps the statistics
	b := newTestApp(t)
	b.Config.GroupStatsFile = a.Config.GroupStatsFile
	if !b.groupStats().starved("tinyrip", 12) {
		t.Error("group statistics not persisted")
	}

	// the upgrade search passes over the starved group when the app approved something else, not otherwise
	pick := func(rs []arrRelease) []string {
		var grabbed []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(rs)
				return
			}
			var body struct {
				GUID string `json:"guid"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			grabbed = append(grabbed, body.GUID)
		}))
		defer s.Close()
		a.upgradeOne(NewArr(s.URL, "k"), upgradeItem{App: "Sonarr", Name: "Show S01E05", Query: "episodeId=5"}, false)
		return grabbed
	}
	tiny := rel("tiny", 100, true, 1080)
	tiny.ReleaseGroup = "TinyRip"
	other := rel("other", 50, true, 1080)
	if got := pick([]arrRelease{tiny, other}); strings.Join(got, ",") != "other" {
		t.Errorf("grabbed %v, want other (TinyRip passed over)", got)
	}
	if got := pick([]arrRelease{tiny}); strings.Join(got, ",") != "tiny" {
		t.Errorf("grabbed %v, want tiny (the only approved release)", got)
	}
}
