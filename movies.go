package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Movies tagged MOVIE_ARCHIVE_TAG (default "archive") in Radarr go into the archive: the movie file and its subtitles
// are copied into MOVIE_ARCHIVE_DIR (the root of Video/Movies, where the owner sorts them into genres), then the movie
// is deleted in Radarr with its download files and an import exclusion, so no list adds it again.

var subtitleExts = []string{".srt", ".ass", ".ssa", ".sub", ".idx", ".vtt"}

type radarrMovie struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Tags      []int  `json:"tags"`
	HasFile   bool   `json:"hasFile"`
	MovieFile *struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"movieFile"`
}

// movieFiles returns the movie file and the subtitles next to it that share its name.
func movieFiles(path string) ([]string, error) {
	files := []string{path}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && n != filepath.Base(path) && strings.HasPrefix(n, stem) &&
			slices.Contains(subtitleExts, strings.ToLower(filepath.Ext(n))) {
			files = append(files, filepath.Join(filepath.Dir(path), n))
		}
	}
	return files, nil
}

// ArchiveMovies moves the Radarr movies tagged for the archive. Runs every CHECK_INTERVAL.
func (a *App) ArchiveMovies() {
	if a.Radarr == nil || a.Config.MovieArchiveDir == "" {
		return
	}
	tags, err := a.Radarr.TagMap()
	if err != nil {
		log.Printf("Archive: %v", err)
		return
	}
	tagID := -1
	for id, label := range tags {
		if label == a.Config.MovieArchiveTag {
			tagID = id
		}
	}
	if tagID < 0 {
		return
	}
	var movies []radarrMovie
	if err := a.Radarr.do("GET", "/api/v3/movie", nil, &movies); err != nil {
		log.Printf("Archive: %v", err)
		return
	}
	for _, m := range movies {
		if !slices.Contains(m.Tags, tagID) {
			continue
		}
		name := fmt.Sprintf("%s (%d)", m.Title, m.Year)
		if !m.HasFile || m.MovieFile == nil {
			a.debugf("Archive: %s has no file yet", name)
			continue
		}
		if err := a.deletable(m.MovieFile.Path); err != nil {
			log.Printf("Archive: refusing %s: %v", name, err)
			continue
		}
		files, err := movieFiles(m.MovieFile.Path)
		if err != nil {
			log.Printf("Archive: %s: %v", name, err)
			continue
		}
		if a.Config.DryRun {
			log.Printf("[dry run] Archive: would copy %d file(s) of %s to %s and remove it from Radarr", len(files), name, a.Config.MovieArchiveDir)
			continue
		}
		if err := a.copyToArchive(files); err != nil {
			log.Printf("Archive: %s: %v", name, err)
			continue
		}
		if err := a.Radarr.do("DELETE", fmt.Sprintf("/api/v3/movie/%d?deleteFiles=true&addImportExclusion=true", m.ID), nil, nil); err != nil {
			log.Printf("Archive: %s copied, but removing it from Radarr failed: %v", name, err)
			continue
		}
		log.Printf("Archive: moved %s to %s (%d file(s)); removed from Radarr with an exclusion", name, a.Config.MovieArchiveDir, len(files))
	}
}

// copyToArchive copies files into the archive root. An existing file of a different size is a name clash: nothing
// is copied then, so the owner can sort it out.
func (a *App) copyToArchive(files []string) error {
	for _, f := range files {
		dst := filepath.Join(a.Config.MovieArchiveDir, filepath.Base(f))
		st, err := os.Stat(f)
		if err != nil {
			return err
		}
		if d, err := os.Stat(dst); err == nil && d.Size() != st.Size() {
			return fmt.Errorf("%s already exists in the archive with a different size", filepath.Base(f))
		}
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			return err
		}
		if err := copyFile(f, filepath.Join(a.Config.MovieArchiveDir, filepath.Base(f)), st.Size()); err != nil {
			return err
		}
	}
	return nil
}
