package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRelease(t *testing.T) {
	cases := []struct {
		name          string
		show          string
		season, epnum int
	}{
		{"futurama.s14e10.1080p.web.h264-cakes[EZTVx.to].mkv", "futurama", 14, 10},
		{"Star.Trek.Strange.New.Worlds.S04E08.1080p.WEB.h264-ETHEL[EZTVx.to].mkv", "startrekstrangenewworlds", 4, 8},
		{"The Great British Bake Off S17E01 Cake Week 1080p ALL4 WEB-DL AAC2 0 H 264-RAWR[EZTVx.to].mkv", "greatbritishbakeoff", 17, 1},
		{"Lanterns - S01E02 - Trust Fall.mkv", "lanterns", 1, 2},
		{"Hells.Kitchen.US.S25E01.1080p.HEVC.x265-MeGusta.mkv", "hellskitchenus", 25, 1},
		{"Futurama - 1x01 - Space Pilot 3000.avi", "futurama", 1, 1},
		{"Family.Guy.S25E00.Happy.Hell-o-ween.1080p.WEB.H264-RVKD[EZTVx.to].mkv", "familyguy", 25, 0},
	}
	for _, c := range cases {
		show, s, e, ok := parseRelease(c.name)
		if !ok || show != c.show || s != c.season || e != c.epnum {
			t.Errorf("parseRelease(%q) = %q %d %d %v, want %q %d %d", c.name, show, s, e, ok, c.show, c.season, c.epnum)
		}
	}
	if _, _, _, ok := parseRelease("Coyote vs Acme 2026 2160p iT WEB-DL.mkv"); ok {
		t.Error("a movie must not parse as an episode")
	}
}

func TestFindEpisodeFiles(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) string {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0755)
		os.WriteFile(full, []byte("x"), 0644)
		return full
	}
	want := mk("ravi/futurama.s14e10.1080p.web.h264-cakes[EZTVx.to].mkv")
	mk("ravi/futurama.s14e09.1080p.web.h264-cakes.mkv")            // other episode
	mk("ravi/futurama.s14e10.1080p.web.h264-cakes.nfo")            // not a video
	mk("ravi/@eaDir/futurama.s14e10.1080p.web.h264-cakes.mkv")     // Synology thumbnails
	mk("ravi/Futurama Revisited S14E10.mkv")                       // different show
	nested := mk("daniela/Futurama S14 1080p/Futurama.S14E10.mkv") // inside a season pack folder
	got := findEpisodeFiles([]string{filepath.Join(root, "ravi"), filepath.Join(root, "daniela")}, "Futurama", 14, 10)
	if len(got) != 2 || got[0] != want || got[1] != nested {
		t.Fatalf("found %v", got)
	}
}

func nameRuleApp(t *testing.T) (*App, string) {
	a := newTestApp(t)
	dl := t.TempDir()
	a.Config.SearchDirs = []string{dl}
	a.Config.ArchiveShows = []string{"Futurama"}
	a.Config.DeleteShows = []string{"Hell's Kitchen (US)"}
	return a, dl
}

func TestArchiveShowByName(t *testing.T) {
	a, dl := nameRuleApp(t)
	src := filepath.Join(dl, "futurama.s14e10.1080p.web.h264-cakes[EZTVx.to].mkv")
	os.WriteFile(src, []byte("episode"), 0644)
	watch(a, "Futurama", 14, 10, 25*time.Hour)
	a.ProcessDue()
	dst := filepath.Join(a.Config.ArchiveDir, "Futurama", "futurama.s14e10.1080p.web.h264-cakes[EZTVx.to].mkv")
	if data, err := os.ReadFile(dst); err != nil || string(data) != "episode" {
		t.Fatalf("archive: %q %v", data, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("download copy should be removed after archiving")
	}
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("queue %+v", a.Queue.Items())
	}
}

func TestDeleteShowByName(t *testing.T) {
	a, dl := nameRuleApp(t)
	src := filepath.Join(dl, "Hells.Kitchen.US.S25E01.1080p.HEVC.x265-MeGusta.mkv")
	keep := filepath.Join(dl, "Hells.Kitchen.US.S25E02.1080p.HEVC.x265-MeGusta.mkv")
	os.WriteFile(src, []byte("x"), 0644)
	os.WriteFile(keep, []byte("x"), 0644)
	watch(a, "Hell's Kitchen (US)", 25, 1, 25*time.Hour)
	a.ProcessDue()
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("watched episode should be deleted")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("other episode must stay")
	}
}

func TestUnlistedShowIsKept(t *testing.T) {
	a, dl := nameRuleApp(t)
	src := filepath.Join(dl, "MobLand.S02E03.1080p.mkv")
	os.WriteFile(src, []byte("x"), 0644)
	watch(a, "MobLand", 2, 3, 25*time.Hour)
	a.ProcessDue()
	if _, err := os.Stat(src); err != nil {
		t.Fatal("unlisted show must not be touched")
	}
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("queue %+v", a.Queue.Items())
	}
}

func TestNameRulesDryRun(t *testing.T) {
	a, dl := nameRuleApp(t)
	a.Config.DryRun = true
	src := filepath.Join(dl, "futurama.s14e10.mkv")
	os.WriteFile(src, []byte("x"), 0644)
	watch(a, "Futurama", 14, 10, 25*time.Hour)
	a.ProcessDue()
	if _, err := os.Stat(src); err != nil {
		t.Fatal("dry run must not move")
	}
	if _, err := os.Stat(filepath.Join(a.Config.ArchiveDir, "Futurama")); !os.IsNotExist(err) {
		t.Fatal("dry run must not archive")
	}
}

func TestSonarrShowTakesPrecedenceOverNameRules(t *testing.T) {
	a, fs := appWithSonarr(t)
	dl := t.TempDir()
	a.Config.SearchDirs = []string{dl}
	a.Config.ArchiveShows = []string{"The Simpsons"} // conflicting name rule must be ignored for Sonarr shows
	os.WriteFile(filepath.Join(dl, "The.Simpsons.S38E02.mkv"), []byte("x"), 0644)
	watch(a, "The Simpsons", 38, 2, 25*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 1 {
		t.Fatalf("Sonarr rule not applied: deleted %v", fs.deleted)
	}
	if _, err := os.Stat(filepath.Join(dl, "The.Simpsons.S38E02.mkv")); err != nil {
		t.Fatal("file outside Sonarr must be untouched")
	}
}

func TestNameRuleAppliesWhenSonarrHasNoFile(t *testing.T) {
	a, fs := appWithSonarr(t)
	dl := t.TempDir()
	a.Config.SearchDirs = []string{dl}
	a.Config.ArchiveDir = t.TempDir()
	a.Config.ArchiveShows = []string{"The Simpsons"} // in Sonarr (delete tag), but this episode came from an RSS rule
	src := filepath.Join(dl, "The.Simpsons.S38E09.1080p.mkv")
	os.WriteFile(src, []byte("x"), 0644)
	watch(a, "The Simpsons", 38, 9, 25*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 {
		t.Fatalf("nothing to delete in Sonarr, got %v", fs.deleted)
	}
	if _, err := os.Stat(filepath.Join(a.Config.ArchiveDir, "The Simpsons", "The.Simpsons.S38E09.1080p.mkv")); err != nil {
		t.Fatalf("name rule not applied: %v", err)
	}
	if len(a.Queue.Items()) != 0 {
		t.Fatal("item should leave the queue")
	}
}

func TestSonarrTagAppliesToFilesFoundByName(t *testing.T) {
	a, fs := appWithSonarr(t)
	dl := t.TempDir()
	a.Config.SearchDirs = []string{dl}
	a.Config.ArchiveDir = t.TempDir()
	a.Config.AnimeArchiveDir = t.TempDir()
	// downloaded by RSS rules before the shows moved to Sonarr: Sonarr has no file for these episodes
	for _, n := range []string{"The.Simpsons.S38E05.1080p.mkv", "Futurama S14E01 1080p.mkv", "Frieren.S02E03.1080p.mkv", "Greys.Anatomy.S23E01.mkv"} {
		os.WriteFile(filepath.Join(dl, n), []byte("x"), 0644)
	}
	watch(a, "The Simpsons", 38, 5, 25*time.Hour) // delete-after-watch
	watch(a, "Futurama", 14, 1, 25*time.Hour)     // archive
	watch(a, "Frieren", 2, 3, 25*time.Hour)       // archive, anime
	watch(a, "Grey's Anatomy", 23, 1, 25*time.Hour)
	a.ProcessDue()
	if _, err := os.Stat(filepath.Join(dl, "Greys.Anatomy.S23E01.mkv")); err != nil {
		t.Error("unmonitored show (tags from an import list): file must be kept")
	}
	if _, err := os.Stat(filepath.Join(dl, "The.Simpsons.S38E05.1080p.mkv")); !os.IsNotExist(err) {
		t.Error("tagged delete-after-watch: file should be deleted")
	}
	if _, err := os.Stat(filepath.Join(a.Config.ArchiveDir, "Futurama", "Season 14", "Futurama S14E01 1080p.mkv")); err != nil {
		t.Errorf("tagged archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Config.AnimeArchiveDir, "Frieren", "Season 02", "Frieren.S02E03.1080p.mkv")); err != nil {
		t.Errorf("anime goes to the anime archive: %v", err)
	}
	if len(fs.deleted) != 0 || len(a.Queue.Items()) != 0 {
		t.Errorf("sonarr deletes %v, queue %v", fs.deleted, a.Queue.Items())
	}
}

func TestReleaseShowName(t *testing.T) {
	for in, want := range map[string]string{
		"The Great British Bake Off S17E01 Cake Week":              "The Great British Bake Off",
		"Bad.Monkey.2024.S01E09.1080p.ATVP.WEB-DL":                 "Bad Monkey",
		"star.trek.strange.new.worlds.s04e05.1080p.web.h264-cakes": "star trek strange new worlds",
		"Lanterns - S01E02 - Trust Fall":                           "Lanterns",
		"1883 S01E01":                                              "1883",
		"Some Movie (2010)":                                        "Some Movie (2010)",
	} {
		if got := releaseShowName(in); got != want {
			t.Errorf("releaseShowName(%q) = %q, want %q", in, got, want)
		}
	}
	if !sameShow("badmonkey2024", "badmonkey") || sameShow("badmonkey2024", "bad") || !sameShow("lucky", "lucky") {
		t.Error("sameShow")
	}
}
