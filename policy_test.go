package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSonarr serves a tiny Sonarr: The Simpsons (delete-after-watch), Futurama (archive), Family Guy (no tag).
type fakeSonarr struct {
	mu        sync.Mutex
	files     map[int]SonarrEpisodeFile // episode file id -> file
	deleted   []int
	unmonitor []int
	fail      bool
}

func newFakeSonarr(t *testing.T, mediaDir string) (*fakeSonarr, *httptest.Server) {
	t.Helper()
	write := func(name, content string) string {
		p := filepath.Join(mediaDir, name)
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := &fakeSonarr{files: map[int]SonarrEpisodeFile{
		11: {ID: 11, Path: write("tv/The Simpsons/Season 38/The Simpsons - S38E02.mkv", "simpsons"), Size: 8},
		21: {ID: 21, Path: write("tv/Futurama/Season 11/Futurama - S11E10.mkv", "futurama!"), Size: 9},
		31: {ID: 31, Path: write("tv/Family Guy/Season 24/Family Guy - S24E01.mkv", "fg"), Size: 2},
	}}
	series := []map[string]any{
		{"id": 1, "title": "The Simpsons", "path": "/downloads/tv/The Simpsons", "tags": []int{7}, "monitored": true},
		{"id": 2, "title": "Futurama", "path": "/downloads/tv/Futurama", "tags": []int{8}, "monitored": true,
			"alternateTitles": []map[string]any{{"title": "Futurama (Hulu)"}}},
		{"id": 3, "title": "Family Guy", "path": "/downloads/tv/Family Guy", "tags": []int{}},
		{"id": 4, "title": "Frieren", "path": "/downloads/tv/Frieren", "tags": []int{8}, "seriesType": "anime", "monitored": true},
		{"id": 5, "title": "Grey's Anatomy", "path": "/downloads/tv/Grey's Anatomy", "tags": []int{7}}, // import list, unmonitored
	}
	episodes := map[int][]SonarrEpisode{
		1: {{ID: 101, SeasonNumber: 38, EpisodeNumber: 2, HasFile: true, EpisodeFileID: 11}},
		2: {{ID: 201, SeasonNumber: 11, EpisodeNumber: 10, HasFile: true, EpisodeFileID: 21}},
		3: {{ID: 301, SeasonNumber: 24, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 31}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if f.fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		enc := json.NewEncoder(w)
		switch {
		case r.URL.Path == "/api/v3/series":
			enc.Encode(series)
		case r.URL.Path == "/api/v3/tag":
			enc.Encode([]SonarrTag{{ID: 7, Label: "delete-after-watch"}, {ID: 8, Label: "archive"}})
		case r.URL.Path == "/api/v3/episode":
			var id int
			fmt.Sscan(r.URL.Query().Get("seriesId"), &id)
			enc.Encode(episodes[id])
		case r.URL.Path == "/api/v3/episode/monitor" && r.Method == http.MethodPut:
			var body struct {
				EpisodeIDs []int `json:"episodeIds"`
				Monitored  bool  `json:"monitored"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if !body.Monitored {
				f.unmonitor = append(f.unmonitor, body.EpisodeIDs...)
			}
			w.WriteHeader(http.StatusAccepted)
		case strings.HasPrefix(r.URL.Path, "/api/v3/episodefile/"):
			var id int
			fmt.Sscan(strings.TrimPrefix(r.URL.Path, "/api/v3/episodefile/"), &id)
			file, ok := f.files[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodDelete {
				os.Remove(file.Path)
				delete(f.files, id)
				f.deleted = append(f.deleted, id)
				return
			}
			enc.Encode(file)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func appWithSonarr(t *testing.T) (*App, *fakeSonarr) {
	a := newTestApp(t)
	fs, srv := newFakeSonarr(t, t.TempDir())
	a.Sonarr = NewSonarr(srv.URL, "key")
	return a, fs
}

func watch(a *App, series string, season, episode int, ago time.Duration) {
	a.Queue.Add(WatchEvent{Source: "plex", Type: "episode", Series: series, Season: season, Episode: episode}, a.now().Add(-ago))
}

func TestDeleteAfterWatch(t *testing.T) {
	a, fs := appWithSonarr(t)
	path := fs.files[11].Path
	watch(a, "The Simpsons", 38, 2, 25*time.Hour)
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[11]" || fmt.Sprint(fs.unmonitor) != "[101]" {
		t.Fatalf("deleted %v, unmonitored %v", fs.deleted, fs.unmonitor)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("queue %+v", a.Queue.Items())
	}
}

func TestArchiveCopiesThenDeletes(t *testing.T) {
	a, fs := appWithSonarr(t)
	watch(a, "Futurama", 11, 10, 30*time.Hour)
	a.ProcessDue()
	dst := filepath.Join(a.Config.ArchiveDir, "Futurama", "Season 11", "Futurama - S11E10.mkv")
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "futurama!" {
		t.Fatalf("archived copy: %q %v", data, err)
	}
	if fmt.Sprint(fs.deleted) != "[21]" || fmt.Sprint(fs.unmonitor) != "[201]" {
		t.Fatalf("deleted %v, unmonitored %v", fs.deleted, fs.unmonitor)
	}
	if _, err := os.Stat(dst + ".partial"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestAlternateTitleMatches(t *testing.T) {
	a, fs := appWithSonarr(t)
	watch(a, "Futurama (Hulu)", 11, 10, 30*time.Hour)
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[21]" {
		t.Fatalf("deleted %v", fs.deleted)
	}
}

func TestGracePeriodNotOver(t *testing.T) {
	a, fs := appWithSonarr(t)
	watch(a, "The Simpsons", 38, 2, 23*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 || len(a.Queue.Items()) != 1 {
		t.Fatalf("deleted %v, queue %+v", fs.deleted, a.Queue.Items())
	}
}

func TestUntaggedAndUnknownShowsAreKept(t *testing.T) {
	a, fs := appWithSonarr(t)
	watch(a, "Family Guy", 24, 1, 48*time.Hour)
	watch(a, "MobLand", 2, 3, 48*time.Hour) // not in Sonarr (still on qBittorrent RSS rules)
	a.ProcessDue()
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v", fs.deleted)
	}
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("handled items should leave the queue: %+v", a.Queue.Items())
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	a, fs := appWithSonarr(t)
	a.Config.DryRun = true
	watch(a, "The Simpsons", 38, 2, 25*time.Hour)
	watch(a, "Futurama", 11, 10, 25*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 || len(fs.unmonitor) != 0 {
		t.Fatalf("deleted %v, unmonitored %v", fs.deleted, fs.unmonitor)
	}
	if _, err := os.Stat(filepath.Join(a.Config.ArchiveDir, "Futurama")); !os.IsNotExist(err) {
		t.Fatal("dry run must not archive")
	}
}

func TestSonarrErrorKeepsItemForRetry(t *testing.T) {
	a, fs := appWithSonarr(t)
	fs.fail = true
	watch(a, "The Simpsons", 38, 2, 25*time.Hour)
	a.ProcessDue()
	items := a.Queue.Items()
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("queue %+v", items)
	}
	fs.fail = false
	a.ProcessDue()
	if len(a.Queue.Items()) != 0 || fmt.Sprint(fs.deleted) != "[11]" {
		t.Fatalf("retry: queue %+v deleted %v", a.Queue.Items(), fs.deleted)
	}
}

func TestQueueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json")
	q, _ := LoadQueue(path)
	at := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	q.Add(WatchEvent{Series: "The Simpsons", Season: 38, Episode: 2, Source: "plex"}, at)
	q2, err := LoadQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	items := q2.Items()
	if len(items) != 1 || !items[0].WatchedAt.Equal(at) {
		t.Fatalf("reloaded %+v", items)
	}
	if due := q2.Due(at.Add(24*time.Hour), 24*time.Hour); len(due) != 1 {
		t.Fatalf("due %+v", due)
	}
}
