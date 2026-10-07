package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestArchiveMoviesMovesTaggedMovies(t *testing.T) {
	dl := t.TempDir()
	write := func(p, s string) string {
		full := filepath.Join(dl, p)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte(s), 0644)
		return full
	}
	keep := write("ravi/movies/Heat (1995)/Heat (1995) (Bluray-1080p).mkv", "movie")
	write("ravi/movies/Heat (1995)/Heat (1995) (Bluray-1080p).en.srt", "sub")
	write("ravi/movies/Heat (1995)/Heat (1995) (Bluray-1080p).nfo", "junk")
	other := write("ravi/movies/Other (2020)/Other (2020).mkv", "other")

	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/tag":
			json.NewEncoder(w).Encode([]SonarrTag{{ID: 3, Label: "1-mallox"}, {ID: 9, Label: "archive"}})
		case r.URL.Path == "/api/v3/movie" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "title": "Heat", "year": 1995, "tags": []int{3, 9}, "hasFile": true, "movieFile": map[string]any{"path": keep, "size": 5}},
				{"id": 2, "title": "Other", "year": 2020, "tags": []int{3}, "hasFile": true, "movieFile": map[string]any{"path": other, "size": 5}},
				{"id": 3, "title": "Waiting", "year": 2026, "tags": []int{9}, "hasFile": false},
			})
		case strings.HasPrefix(r.URL.Path, "/api/v3/movie/") && r.Method == http.MethodDelete:
			if r.URL.Query().Get("deleteFiles") != "true" || r.URL.Query().Get("addImportExclusion") != "true" {
				t.Errorf("delete query %s", r.URL.RawQuery)
			}
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/api/v3/movie/"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := newTestApp(t)
	a.Radarr = NewArr(srv.URL, "k")
	a.Config.MovieArchiveDir = t.TempDir()
	a.Config.MovieArchiveTag = "archive"
	a.Config.DeleteRoots = []string{dl}

	a.Config.DryRun = true
	a.ArchiveMovies()
	if len(deleted) != 0 {
		t.Fatal("dry run deleted")
	}
	a.Config.DryRun = false
	a.ArchiveMovies()
	for _, f := range []string{"Heat (1995) (Bluray-1080p).mkv", "Heat (1995) (Bluray-1080p).en.srt"} {
		if _, err := os.Stat(filepath.Join(a.Config.MovieArchiveDir, f)); err != nil {
			t.Errorf("%s not archived: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(a.Config.MovieArchiveDir, "Heat (1995) (Bluray-1080p).nfo")); err == nil {
		t.Error("only the movie and its subtitles go to the archive")
	}
	if strings.Join(deleted, ",") != "1" {
		t.Errorf("removed from Radarr: %v, want only the tagged movie with a file", deleted)
	}

	// a movie outside the delete roots is left alone entirely
	deleted = nil
	a.Config.DeleteRoots = []string{"/downloads"}
	os.RemoveAll(a.Config.MovieArchiveDir)
	os.MkdirAll(a.Config.MovieArchiveDir, 0755)
	a.ArchiveMovies()
	if len(deleted) != 0 {
		t.Errorf("deleted %v outside the delete roots", deleted)
	}
	if entries, _ := os.ReadDir(a.Config.MovieArchiveDir); len(entries) != 0 {
		t.Error("copied although the source can't be removed")
	}
}

func TestArchiveNameClashCopiesNothing(t *testing.T) {
	a := newTestApp(t)
	a.Config.MovieArchiveDir = t.TempDir()
	src := filepath.Join(t.TempDir(), "Heat (1995).mkv")
	os.WriteFile(src, []byte("new copy"), 0644)
	os.WriteFile(filepath.Join(a.Config.MovieArchiveDir, "Heat (1995).mkv"), []byte("old"), 0644)
	if err := a.copyToArchive([]string{src}); err == nil {
		t.Error("a different file with the same name must not be overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(a.Config.MovieArchiveDir, "Heat (1995).mkv")); string(b) != "old" {
		t.Error("archive file changed")
	}
}
