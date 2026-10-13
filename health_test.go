package main

import (
	"encoding/json"
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

type fakeQueue struct {
	mu       sync.Mutex
	items    []map[string]any
	rejected []string
	removed  []string         // dropped without blocklist and re-search
	commands []map[string]any // POST /api/v3/command bodies
}

func newFakeQueue(t *testing.T, items []map[string]any) (*fakeQueue, *httptest.Server) {
	f := &fakeQueue{items: items}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/queue" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"records": f.items})
		case strings.HasPrefix(r.URL.Path, "/api/v3/queue/") && r.Method == http.MethodDelete:
			q, id := r.URL.Query(), strings.TrimPrefix(r.URL.Path, "/api/v3/queue/")
			switch {
			case q.Get("removeFromClient") == "true" && q.Get("blocklist") == "true" && q.Get("skipRedownload") == "false":
				f.rejected = append(f.rejected, id)
			case q.Get("removeFromClient") == "true" && q.Get("blocklist") == "false" && q.Get("skipRedownload") == "true":
				f.removed = append(f.removed, id)
			default:
				t.Errorf("queue delete query %s", r.URL.RawQuery)
			}
		case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
			var c map[string]any
			json.NewDecoder(r.Body).Decode(&c)
			f.commands = append(f.commands, c)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(c)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestCheckDownloadsRejectsFakesAndStalled(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	torrents := []Torrent{
		{Hash: "aaa", Name: "fake", State: "stoppedUP"},
		// no metadata: size unknown, so amount_left is 0 (as qBittorrent reports it)
		{Hash: "bbb", Name: "no metadata", State: "metaDL", AddedOn: ago(14 * time.Hour), LastActivity: ago(14 * time.Hour)},
		{Hash: "ccc", Name: "stalled lately", State: "stalledDL", AmountLeft: 1, AddedOn: ago(30 * time.Hour), LastActivity: ago(2 * time.Hour)},
		{Hash: "ddd", Name: "slow but moving", State: "downloading", AmountLeft: 1, Progress: 0.02, AddedOn: ago(20 * time.Hour), LastActivity: ago(5 * time.Minute)},
		{Hash: "eee", Name: "stalled long", State: "stalledDL", AmountLeft: 1, AddedOn: ago(40 * time.Hour), LastActivity: ago(13 * time.Hour)},
		{Hash: "fff", Name: "connected, no data", State: "downloading", AmountLeft: 1, AddedOn: ago(14 * time.Hour), LastActivity: ago(14 * time.Hour)},
		{Hash: "ggg", Name: "queued", State: "queuedDL", AmountLeft: 1, AddedOn: ago(40 * time.Hour)},
	}
	msg := func(m string) []map[string]any { return []map[string]any{{"messages": []string{m}}} }
	_, qsrv := newFakeQbt(t, torrents, nil)
	fs, ssrv := newFakeQueue(t, []map[string]any{
		{"id": 1, "title": "Ted Lasso S04E10", "downloadId": "AAA", "protocol": "torrent", "trackedDownloadState": "importPending",
			"statusMessages": msg("Caution: Found executable file with extension: '.exe'")},
		{"id": 2, "title": "Bad Monkey S01E04", "downloadId": "BBB", "protocol": "torrent", "trackedDownloadState": "downloading"},
		{"id": 3, "title": "Recent activity", "downloadId": "CCC", "protocol": "torrent", "trackedDownloadState": "downloading"},
		{"id": 4, "title": "Fine", "downloadId": "DDD", "protocol": "torrent", "trackedDownloadState": "downloading"},
		{"id": 5, "title": "Import pending, normal", "downloadId": "XXX", "protocol": "torrent", "trackedDownloadState": "importPending",
			"statusMessages": msg("Episode has a TBA title and recently aired")},
		{"id": 6, "title": "Bad Monkey S01E03", "downloadId": "FFF", "protocol": "torrent", "trackedDownloadState": "downloading"},
		{"id": 7, "title": "Waiting for a slot", "downloadId": "GGG", "protocol": "torrent", "trackedDownloadState": "downloading"},
	})
	fr, rsrv := newFakeQueue(t, []map[string]any{
		{"id": 9, "title": "Me Time", "downloadId": "EEE", "protocol": "torrent", "trackedDownloadState": "downloading"},
	})
	a := newTestApp(t)
	a.Now = func() time.Time { return now }
	a.Config.StallTimeout = 12 * time.Hour
	a.Config.SweepMaxRemove = 5
	a.Qbt = NewQbittorrent(qsrv.URL, "", "")
	a.Sonarr = NewSonarr(ssrv.URL, "k")
	a.Radarr = NewArr(rsrv.URL, "k")
	a.CheckDownloads()
	sort.Strings(fs.rejected)
	if strings.Join(fs.rejected, ",") != "1,2,6" || strings.Join(fr.rejected, ",") != "9" {
		t.Errorf("rejected sonarr %v radarr %v, want the fake and the long-stalled downloads only", fs.rejected, fr.rejected)
	}

	// dry run and the per-run limit
	fs.rejected, fr.rejected = nil, nil
	a.Config.DryRun = true
	a.CheckDownloads()
	a.Config.DryRun, a.Config.SweepMaxRemove = false, 1
	a.CheckDownloads()
	if len(fs.rejected)+len(fr.rejected) != 1 {
		t.Errorf("dry run or limit ignored: %v %v", fs.rejected, fr.rejected)
	}
}

// Always Sunny S18E10, 2026-10-12: Sonarr moved the file into the series folder, the database write failed ("database
// is locked"), and the queue said "No files found are eligible". Such downloads are rescanned, not blocklisted.
func TestCheckDownloadsHalfFailedImport(t *testing.T) {
	dl := filepath.Join(t.TempDir(), "sonarr")
	mk := func(rel string) {
		p := filepath.Join(dl, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("extras only/show.nfo")
	mk("extras only/Screens/1.jpg")
	mk("fake/show.exe")
	mk("still here.mkv")
	noFiles := []map[string]any{{"messages": []string{"No files found are eligible for import in /downloads/sonarr/x"}}}
	item := func(id int, hash, out string) map[string]any {
		return map[string]any{"id": id, "title": out, "downloadId": hash, "protocol": "torrent", "trackedDownloadState": "importBlocked",
			"outputPath": filepath.Join(dl, out), "seriesId": 4, "statusMessages": noFiles}
	}
	var torrents []Torrent
	for _, h := range []string{"a1", "a2", "a3", "a4"} {
		torrents = append(torrents, Torrent{Hash: h, State: "stoppedUP", Progress: 1, SavePath: dl})
	}
	_, qsrv := newFakeQbt(t, torrents, nil)
	fs, ssrv := newFakeQueue(t, []map[string]any{
		item(1, "A1", "single file moved.mkv"), // gone
		item(2, "A2", "extras only"),           // folder without the video
		item(3, "A3", "fake"),                  // an .exe: a real fake
		item(4, "A4", "still here.mkv"),        // video still there: not imported for another reason
	})
	a := newTestApp(t)
	a.Config.SweepMaxRemove = 5
	a.Qbt = NewQbittorrent(qsrv.URL, "", "")
	a.Sonarr = NewSonarr(ssrv.URL, "k")
	a.CheckDownloads()
	sort.Strings(fs.removed)
	sort.Strings(fs.rejected)
	if strings.Join(fs.removed, ",") != "1,2" || strings.Join(fs.rejected, ",") != "3,4" {
		t.Errorf("removed %v rejected %v, want moved downloads removed and the rest rejected", fs.removed, fs.rejected)
	}
	if len(fs.commands) != 2 || fs.commands[0]["name"] != "RescanSeries" || fs.commands[0]["seriesId"] != float64(4) {
		t.Errorf("commands %v, want one RescanSeries per moved download", fs.commands)
	}
	if alreadyMoved(filepath.Join(t.TempDir(), "unmounted", "show.mkv")) {
		t.Error("a missing parent (unmounted share) must not count as moved")
	}
}

func TestPlexCollectionMovies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept %q", r.Header.Get("Accept"))
		}
		dir := func(key, typ, path string) map[string]any {
			return map[string]any{"key": key, "type": typ, "Location": []map[string]any{{"path": path}}}
		}
		movie := func(ids ...string) map[string]any {
			var g []map[string]any
			for _, id := range ids {
				g = append(g, map[string]any{"id": id})
			}
			return map[string]any{"guid": "plex://movie/5d776", "Guid": g} // Plex sends both keys
		}
		var mc any
		switch r.URL.Path {
		case "/library/sections":
			mc = map[string]any{"Directory": []any{dir("4", "movie", "/volume1/Video/Movies"), dir("15", "movie", "/volume1/Downloads/ravi"),
				dir("6", "show", "/volume1/Video/Series")}}
		case "/library/sections/4/all":
			mc = map[string]any{"Metadata": []any{movie("imdb://tt1013743", "tmdb://37834"), movie("tmdb://155"), movie()}}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": mc})
	}))
	defer srv.Close()
	ids, err := NewPlex(srv.URL, "/volume1/Video/").CollectionMovies()
	if err != nil || len(ids) != 2 || !ids[37834] || !ids[155] {
		t.Fatalf("ids %v err %v", ids, err)
	}
}
