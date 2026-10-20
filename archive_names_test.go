package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveEpisodeName(t *testing.T) {
	for in, want := range map[string]string{
		"Barry - S01E01 - Chapter One WEBDL-1080p.mkv":      "Barry - S01E01 - Chapter One.mkv",
		"Loki - S02E03 - 1893 HDTV-720p Proper.mkv":         "Loki - S02E03 - 1893.mkv",
		"Show - S01E02 - Title Bluray-1080p Remux.mkv":      "Show - S01E02 - Title.mkv",
		"Futurama S14E01 1080p.mkv":                         "Futurama S14E01 1080p.mkv",
		"Invincible - S04E01 - Making the World Better.mkv": "Invincible - S04E01 - Making the World Better.mkv",
	} {
		if got := archiveEpisodeName(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestCleanArchiveTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Mission: Impossible - The Final Reckoning":          "Mission Impossible - The Final Reckoning",
		"Avengers: Doomsday":                                 "Avengers - Doomsday",
		"Dr. Strangelove or: How I Learned to Stop Worrying": "Dr. Strangelove or How I Learned to Stop Worrying",
		"What If…?": "What If…",
	} {
		if got := cleanArchiveTitle(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestSeriesTitleContinuesNumbering(t *testing.T) {
	mi := []archivedFilm{{"Mission Impossible 1 (1996)", 1996}, {"Mission Impossible 6 - Fallout (2018) [Commentary]", 2018},
		{"Mission Impossible 7 - Dead Reckoning (2023)", 2023}}
	blade := []archivedFilm{{"Blade I (1998)", 1998}, {"Blade II (2002)", 2002}, {"Blade III - Trinity (2004)", 2004}}
	for _, c := range []struct {
		official string
		year     int
		siblings []archivedFilm
		want     string
	}{
		{"Mission: Impossible - The Final Reckoning", 2025, mi, "Mission Impossible 8 - The Final Reckoning"},
		{"Blade", 2027, blade, "Blade IV"},
		{"Mission: Impossible - Prequel", 1990, mi, "Mission Impossible - Prequel"},                                         // older than the series: official title
		{"Furiosa: A Mad Max Saga", 2024, []archivedFilm{{"Mad Max - Fury Road (2015)", 2015}}, "Furiosa - A Mad Max Saga"}, // series not numbered
		{"Heat", 1995, nil, "Heat"},
	} {
		if got := seriesTitle(c.official, c.year, c.siblings); got != c.want {
			t.Errorf("%s: got %q, want %q", c.official, got, c.want)
		}
	}
}

func TestLabels(t *testing.T) {
	if l := languageLabel([]string{"German"}, "Italian"); l != "Deutsch" {
		t.Errorf("German dub of an Italian film: %q", l)
	}
	if l := languageLabel([]string{"German"}, "German"); l != "" {
		t.Errorf("original language gets no label: %q", l)
	}
	if l := languageLabel([]string{"German", "English"}, "Italian"); l != "" {
		t.Errorf("English audio gets no label: %q", l)
	}
	if e := editionLabel("Extended Edition"); e != "Extended" {
		t.Errorf("edition %q", e)
	}
	if e := editionLabel("Directors Cut"); e != "Director's Cut" {
		t.Errorf("edition %q", e)
	}
	if s := archiveMovieStem("Heat", 1995, []string{"", "Deutsch"}); s != "Heat (1995) [Deutsch]" {
		t.Errorf("stem %q", s)
	}
}

func TestArchiveStemAddsQualityForAnotherVersion(t *testing.T) {
	a := newTestApp(t)
	a.Config.MovieArchiveDir = t.TempDir()
	os.WriteFile(filepath.Join(a.Config.MovieArchiveDir, "Heat (1995).avi"), []byte("old"), 0644)
	var m radarrMovie
	json.Unmarshal([]byte(`{"tmdbId":949,"title":"Heat","year":1995,"movieFile":{"path":"/x/Heat.mkv","quality":{"quality":{"resolution":2160}}}}`), &m)
	stem, other := a.archiveStem(m)
	if stem != "Heat (1995) [4K]" || !other {
		t.Fatalf("got %q other=%v", stem, other)
	}
	m.Title, m.TmdbID = "Ronin", 8195
	if stem, other := a.archiveStem(m); stem != "Ronin (1995)" || other {
		t.Fatalf("got %q other=%v", stem, other)
	}
}

func TestArchiveShowDir(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "Chapelle's Show"), 0755) // the owner's folder, spelled differently from Sonarr
	os.Mkdir(filepath.Join(root, "IT Crowd"), 0755)
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"6","type":"show","Location":[{"path":"/volume1/Video/Series"}]}]}}`))
		case "/library/sections/6/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"5","guid":"plex://x","Guid":[{"id":"tvdb://75417"}]}]}}`))
		case "/library/metadata/5":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Location":[{"path":"/volume1/Video/Series/Chapelle's Show"}]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()
	a := newTestApp(t)
	a.Plex = NewPlex(plex.URL, "/volume1/Video")
	if d := a.archiveShowDir(root, 75417, "Chappelle's Show"); d != filepath.Join(root, "Chapelle's Show") {
		t.Errorf("by TVDB id via Plex: %s", d)
	}
	if d := a.archiveShowDir(root, 0, "The IT Crowd"); d != filepath.Join(root, "IT Crowd") {
		t.Errorf("by name: %s", d)
	}
	if d := a.archiveShowDir(root, 0, "Severance"); d != filepath.Join(root, "Severance") {
		t.Errorf("new show: %s", d)
	}
}

func TestCollectionFilmsAndMergeVersions(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"4","type":"movie","Location":[{"path":"/volume1/Video/Movies"}]}]}}`))
		case "/library/sections/4/collections":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"9","title":"Rambo Collection"},{"ratingKey":"8","title":"Other"}]}}`))
		case "/library/collections/9/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"year":1982,"Media":[{"Part":[{"file":"/volume1/Video/Movies/Rambo 1 (1982).mp4"}]}]}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()
	films, err := NewPlex(plex.URL, "/volume1/Video").CollectionFilms("Rambo Collection")
	if err != nil || len(films) != 1 || films[0].Stem != "Rambo 1 (1982)" || films[0].Year != 1982 {
		t.Fatalf("films %+v %v", films, err)
	}

	var merged string
	n := 3
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Items":
			var items []map[string]any
			for i := 0; i < n; i++ {
				items = append(items, map[string]any{"Id": string(rune('a' + i)), "Path": "/volume1/Video/Movies/Heat (1995).mkv", "ProviderIds": map[string]string{"Tmdb": "949"}})
			}
			items = append(items, map[string]any{"Id": "z", "Path": "/volume1/Downloads/ravi/movies/Heat.mkv", "ProviderIds": map[string]string{"Tmdb": "949"}},
				map[string]any{"Id": "y", "Path": "/volume1/Video/Movies/Ronin (1998).mkv", "ProviderIds": map[string]string{"Tmdb": "8195"}})
			json.NewEncoder(w).Encode(map[string]any{"Items": items})
		case r.URL.Path == "/Videos/MergeVersions" && r.Method == http.MethodPost:
			merged = r.URL.Query().Get("ids")
		default:
			http.NotFound(w, r)
		}
	}))
	defer jf.Close()
	j := NewJellyfin(jf.URL, "k")
	if got, err := j.MergeArchiveVersions(949, "/volume1/Video/Movies"); err != nil || got != 3 || merged != "a,b,c" {
		t.Fatalf("merged %q (%d, %v): only the archive copies of this film", merged, got, err)
	}
	n, merged = 8, ""
	if _, err := j.MergeArchiveVersions(949, "/volume1/Video/Movies"); err == nil || merged != "" || !strings.Contains(err.Error(), "not merging") {
		t.Fatalf("eight items must be refused: %v %q", err, merged)
	}
}
