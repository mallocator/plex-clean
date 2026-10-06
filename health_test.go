package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			q := r.URL.Query()
			if q.Get("removeFromClient") != "true" || q.Get("blocklist") != "true" || q.Get("skipRedownload") != "false" {
				t.Errorf("reject query %s", r.URL.RawQuery)
			}
			f.rejected = append(f.rejected, strings.TrimPrefix(r.URL.Path, "/api/v3/queue/"))
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
		{Hash: "bbb", Name: "no metadata", State: "metaDL", AmountLeft: 1, AddedOn: ago(14 * time.Hour)},
		{Hash: "ccc", Name: "stalled lately", State: "stalledDL", AmountLeft: 1, AddedOn: ago(30 * time.Hour), LastActivity: ago(2 * time.Hour)},
		{Hash: "ddd", Name: "downloading", State: "downloading", AmountLeft: 1, AddedOn: ago(20 * time.Hour)},
		{Hash: "eee", Name: "stalled long", State: "stalledDL", AmountLeft: 1, AddedOn: ago(40 * time.Hour), LastActivity: ago(13 * time.Hour)},
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
	if strings.Join(fs.rejected, ",") != "1,2" || strings.Join(fr.rejected, ",") != "9" {
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
			return map[string]any{"Guid": g}
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
