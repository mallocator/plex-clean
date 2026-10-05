package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Shows that Sonarr doesn't manage (e.g. still downloaded by qBittorrent RSS rules) can be handled by name:
// ARCHIVE_SHOWS / DELETE_SHOWS list show names, and the watched episode's file is found in SEARCH_DIRS by its
// release name ("Show.Name.S14E10...", "show name 1x10 ...").

var videoExts = []string{".mkv", ".mp4", ".avi", ".m4v", ".ts", ".mov", ".wmv"}

// episodeToken finds "s14e10" / "s14.e10" / "14x10" in a normalized (lowercase, space-separated) name.
var episodeToken = regexp.MustCompile(`(?:^|\s)(?:s(\d{1,2})\s?e(\d{1,3})|(\d{1,2})x(\d{2,3}))(?:\s|e\d|$)`)

// parseRelease splits a release file name into a normalized show name, season and episode.
func parseRelease(name string) (show string, season, episode int, ok bool) {
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '\'' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	norm := " " + strings.Join(strings.Fields(b.String()), " ") + " "
	m := episodeToken.FindStringSubmatchIndex(norm)
	if m == nil {
		return "", 0, 0, false
	}
	sub := func(i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return norm[m[2*i]:m[2*i+1]]
	}
	s, e := sub(1), sub(2)
	if s == "" {
		s, e = sub(3), sub(4)
	}
	season, _ = strconv.Atoi(s)
	episode, _ = strconv.Atoi(e)
	return normalizeTitle(norm[:m[0]]), season, episode, true
}

// findEpisodeFiles returns the video files below dirs whose release name matches the show and episode.
func findEpisodeFiles(dirs []string, series string, season, episode int) []string {
	want := normalizeTitle(series)
	var found []string
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entries are skipped
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), "@") || strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir // Synology @eaDir, hidden folders
				}
				return nil
			}
			if !slices.Contains(videoExts, strings.ToLower(filepath.Ext(path))) {
				return nil
			}
			show, s, e, ok := parseRelease(d.Name())
			if !ok || s != season || e != episode {
				return nil
			}
			// The show name may also be in the parent folder only ("Show S01/S01E02.mkv").
			if show == want || (show == "" && strings.HasPrefix(normalizeTitle(filepath.Base(filepath.Dir(path))), want)) {
				found = append(found, path)
			}
			return nil
		})
	}
	return found
}

// applyByName handles a show listed in ARCHIVE_SHOWS or DELETE_SHOWS. Returns handled=false if it isn't listed.
func (a *App) applyByName(it PendingItem) (handled bool, err error) {
	key := normalizeTitle(it.Series)
	var action, archiveName string
	for _, s := range a.Config.ArchiveShows {
		if normalizeTitle(s) == key {
			action, archiveName = "archive", s
		}
	}
	if action == "" {
		for _, s := range a.Config.DeleteShows {
			if normalizeTitle(s) == key {
				action = "delete"
			}
		}
	}
	if action == "" {
		return false, nil
	}
	files := findEpisodeFiles(a.Config.SearchDirs, it.Series, it.Season, it.Episode)
	if len(files) == 0 {
		log.Printf("%s: no file found in %s (already moved or deleted?)", it, strings.Join(a.Config.SearchDirs, ", "))
		return true, nil
	}
	for _, f := range files {
		if action == "archive" {
			dst := filepath.Join(a.Config.ArchiveDir, archiveName, filepath.Base(f))
			if a.Config.DryRun {
				log.Printf("[dry run] %s: would move %s to %s", it, f, dst)
				continue
			}
			st, err := os.Stat(f)
			if err != nil {
				return true, err
			}
			if err := copyFile(f, dst, st.Size()); err != nil {
				return true, fmt.Errorf("archiving to %s: %w", dst, err)
			}
			if err := os.Remove(f); err != nil {
				return true, err
			}
			log.Printf("%s: moved %s to %s", it, f, dst)
		} else {
			if a.Config.DryRun {
				log.Printf("[dry run] %s: would delete %s", it, f)
				continue
			}
			if err := os.Remove(f); err != nil {
				return true, err
			}
			log.Printf("%s: deleted %s", it, f)
		}
	}
	return true, nil
}
