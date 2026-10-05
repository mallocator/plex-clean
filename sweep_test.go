package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

type fakeQbt struct {
	mu       sync.Mutex
	torrents []Torrent
	files    map[string][]TorrentFile
	removed  []string
	logins   int
}

func newFakeQbt(t *testing.T, torrents []Torrent, files map[string][]TorrentFile) (*fakeQbt, *httptest.Server) {
	f := &fakeQbt{torrents: torrents, files: files}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			f.logins++
			w.WriteHeader(http.StatusNoContent) // qBittorrent 5
		case "/api/v2/torrents/info":
			json.NewEncoder(w).Encode(f.torrents)
		case "/api/v2/torrents/files":
			json.NewEncoder(w).Encode(f.files[r.URL.Query().Get("hash")])
		case "/api/v2/torrents/delete":
			r.ParseForm()
			if r.Form.Get("deleteFiles") != "true" {
				t.Errorf("deleteFiles = %q", r.Form.Get("deleteFiles"))
			}
			f.removed = append(f.removed, r.Form.Get("hashes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func sweepApp(t *testing.T, torrents []Torrent, files map[string][]TorrentFile, present ...string) (*App, *fakeQbt) {
	t.Helper()
	root := t.TempDir()
	for _, p := range present {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte("x"), 0644)
	}
	os.MkdirAll(filepath.Join(root, "ravi"), 0755)
	for i := range torrents {
		if torrents[i].SavePath == "" {
			torrents[i].SavePath = filepath.Join(root, "ravi")
		} else if !filepath.IsAbs(torrents[i].SavePath) {
			torrents[i].SavePath = filepath.Join(root, torrents[i].SavePath)
		}
	}
	fq, srv := newFakeQbt(t, torrents, files)
	a := newTestApp(t)
	a.Config.SweepRoot = root
	a.Config.SweepSkipCats = []string{"sonarr", "radarr"}
	a.Config.SweepMaxRemove = 5
	a.Qbt = NewQbittorrent(srv.URL, "admin", "secret")
	return a, fq
}

func TestSweepRemovesOnlyCompletedTorrentsWithMissingFiles(t *testing.T) {
	torrents := []Torrent{
		{Hash: "present", Name: "Present"},
		{Hash: "gone", Name: "Gone"},
		{Hash: "downloading", Name: "Downloading", AmountLeft: 100},
		{Hash: "sonarr", Name: "Sonarr", Category: "sonarr"},
		{Hash: "outside", Name: "Outside", SavePath: "/somewhere/else"},
		{Hash: "unselected", Name: "Unselected"},
	}
	files := map[string][]TorrentFile{
		"present":     {{Name: "present.mkv", Priority: 1}},
		"gone":        {{Name: "gone.mkv", Priority: 1}},
		"downloading": {{Name: "dl.mkv", Priority: 1}},
		"sonarr":      {{Name: "s.mkv", Priority: 1}},
		"outside":     {{Name: "o.mkv", Priority: 1}},
		"unselected":  {{Name: "skipped.nfo", Priority: 0}, {Name: "kept.mkv", Priority: 1}},
	}
	a, fq := sweepApp(t, torrents, files, "ravi/present.mkv", "ravi/kept.mkv")
	a.Sweep()
	if len(fq.removed) != 1 || fq.removed[0] != "gone" {
		t.Fatalf("removed %v, want [gone]", fq.removed)
	}
	if fq.logins != 1 {
		t.Errorf("logins %d", fq.logins)
	}
}

func TestSweepRefusesMassRemoval(t *testing.T) {
	var torrents []Torrent
	files := map[string][]TorrentFile{}
	for _, h := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		torrents = append(torrents, Torrent{Hash: h, Name: h})
		files[h] = []TorrentFile{{Name: h + ".mkv", Priority: 1}}
	}
	a, fq := sweepApp(t, torrents, files, "ravi/other.txt")
	a.Sweep()
	if len(fq.removed) != 0 {
		t.Fatalf("removed %v; a sweep where everything looks deleted must be refused", fq.removed)
	}
}

func TestSweepAllowsLargeCleanupOfAMinority(t *testing.T) {
	var torrents []Torrent
	files := map[string][]TorrentFile{}
	var present []string
	for i := 0; i < 20; i++ {
		h := string(rune('a' + i))
		torrents = append(torrents, Torrent{Hash: h, Name: h})
		files[h] = []TorrentFile{{Name: h + ".mkv", Priority: 1}}
		if i >= 7 { // 7 of 20 deleted
			present = append(present, "ravi/"+h+".mkv")
		}
	}
	a, fq := sweepApp(t, torrents, files, present...)
	a.Sweep()
	sort.Strings(fq.removed)
	if len(fq.removed) != 7 {
		t.Fatalf("removed %v, want 7", fq.removed)
	}
}

func TestSweepSkipsEmptyRoot(t *testing.T) {
	fq, srv := newFakeQbt(t, []Torrent{{Hash: "x", SavePath: "/downloads/ravi"}}, nil)
	a := newTestApp(t)
	a.Config.SweepRoot = t.TempDir() // empty: share not mounted
	a.Qbt = NewQbittorrent(srv.URL, "", "")
	a.Sweep()
	if fq.logins != 0 || len(fq.removed) != 0 {
		t.Fatalf("logins %d removed %v", fq.logins, fq.removed)
	}
}

func TestSweepDryRun(t *testing.T) {
	a, fq := sweepApp(t, []Torrent{{Hash: "gone", Name: "Gone"}}, map[string][]TorrentFile{"gone": {{Name: "gone.mkv", Priority: 1}}}, "ravi/other.txt")
	a.Config.DryRun = true
	a.Sweep()
	if len(fq.removed) != 0 {
		t.Fatalf("removed %v", fq.removed)
	}
}

func TestLoginWithoutCredentialsIsSkipped(t *testing.T) {
	fq, srv := newFakeQbt(t, nil, nil)
	if err := NewQbittorrent(srv.URL, "", "").Login(); err != nil || fq.logins != 0 {
		t.Fatalf("err %v logins %d", err, fq.logins)
	}
}
