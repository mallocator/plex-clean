package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Movies tagged MOVIE_ARCHIVE_TAG (default "archive") in Radarr, or marked as favourite in Jellyfin by one of
// JELLYFIN_ARCHIVE_USERS, go into the archive: the movie file and its subtitles
// are copied into MOVIE_ARCHIVE_DIR (Video/Movies, one flat folder) under the archive name (archive_names.go:
// "<Title> (<Year>) [labels]", continuing the owner's series numbering), then the movie is deleted in Radarr with its
// download files and an import exclusion, so no list adds it again. A film that already has a version in the archive
// gets its versions merged in Jellyfin once Jellyfin has scanned the new file.

var subtitleExts = []string{".srt", ".ass", ".ssa", ".sub", ".idx", ".vtt"}

type radarrMovie struct {
	ID               int    `json:"id"`
	TmdbID           int    `json:"tmdbId"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	Tags             []int  `json:"tags"`
	HasFile          bool   `json:"hasFile"`
	OriginalLanguage struct {
		Name string `json:"name"`
	} `json:"originalLanguage"`
	Collection *struct {
		Title string `json:"title"`
	} `json:"collection"`
	MovieFile *struct {
		Path      string `json:"path"`
		Size      int64  `json:"size"`
		Edition   string `json:"edition"`
		Languages []struct {
			Name string `json:"name"`
		} `json:"languages"`
		Quality struct {
			Quality struct {
				Resolution int `json:"resolution"`
			} `json:"quality"`
		} `json:"quality"`
	} `json:"movieFile"`
}

// archiveStem is the archive name of a Radarr movie (archive_names.go). taken reports a quality label had to be added
// because the title with its labels already exists, i.e. this is another version of a film in the archive.
func (a *App) archiveStem(m radarrMovie) (stem string, other bool) {
	title := cleanArchiveTitle(m.Title)
	if a.Plex != nil && m.Collection != nil && m.Collection.Title != "" {
		if films, err := a.Plex.CollectionFilms(m.Collection.Title); err == nil {
			title = seriesTitle(m.Title, m.Year, films)
		} else {
			log.Printf("Archive: %s: plex collection lookup: %v", m.Title, err)
		}
	}
	var langs []string
	for _, l := range m.MovieFile.Languages {
		langs = append(langs, l.Name)
	}
	labels := []string{editionLabel(m.MovieFile.Edition), languageLabel(langs, m.OriginalLanguage.Name)}
	stem = archiveMovieStem(title, m.Year, labels)
	if archiveStemTaken(a.Config.MovieArchiveDir, stem) {
		return archiveMovieStem(title, m.Year, append(labels, resolutionLabel(m.MovieFile.Quality.Quality.Resolution))), true
	}
	return stem, a.Plex != nil && a.ownedInPlex(m.TmdbID)
}

func (a *App) ownedInPlex(tmdbID int) bool {
	owned, err := a.Plex.CollectionMovies()
	return err == nil && owned[tmdbID]
}

// mergePendingVersions asks Jellyfin to merge the versions of recently archived films; entries go after a success
// or a day.
func (a *App) mergePendingVersions() {
	if a.Jellyfin == nil {
		return
	}
	a.mergeMu.Lock()
	defer a.mergeMu.Unlock()
	for id, since := range a.pendingMerges {
		n, err := a.Jellyfin.MergeArchiveVersions(id, a.Config.MovieArchiveHostPath)
		switch {
		case err != nil:
			log.Printf("Archive: jellyfin version merge for tmdb %d: %v", id, err)
		case n > 1:
			log.Printf("Archive: merged %d versions of tmdb %d in Jellyfin", n, id)
			delete(a.pendingMerges, id)
			continue
		}
		if a.now().Sub(since) > 24*time.Hour {
			delete(a.pendingMerges, id)
		}
	}
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
	a.mergePendingVersions()
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
	favorites := map[int]bool{}
	if a.Jellyfin != nil && len(a.Config.JellyfinArchiveUsers) > 0 {
		if favorites, err = a.Jellyfin.FavoriteMovies(a.Config.JellyfinArchiveUsers); err != nil {
			log.Printf("Archive: jellyfin favourites: %v", err)
			favorites = map[int]bool{}
		}
	}
	if tagID < 0 && len(favorites) == 0 {
		return
	}
	var movies []radarrMovie
	if err := a.Radarr.do("GET", "/api/v3/movie", nil, &movies); err != nil {
		log.Printf("Archive: %v", err)
		return
	}
	for _, m := range movies {
		why := "tagged " + a.Config.MovieArchiveTag
		switch {
		case slices.Contains(m.Tags, tagID):
		case favorites[m.TmdbID]:
			why = "favourite in Jellyfin"
		default:
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
		stem, other := a.archiveStem(m)
		if a.Config.DryRun {
			log.Printf("[dry run] Archive: would copy %d file(s) of %s (%s) to %s as %q and remove it from Radarr", len(files), name, why, a.Config.MovieArchiveDir, stem)
			continue
		}
		if err := a.copyToArchive(files, stem); err != nil {
			log.Printf("Archive: %s: %v", name, err)
			continue
		}
		if err := a.Radarr.do("DELETE", fmt.Sprintf("/api/v3/movie/%d?deleteFiles=true&addImportExclusion=true", m.ID), nil, nil); err != nil {
			log.Printf("Archive: %s copied, but removing it from Radarr failed: %v", name, err)
			continue
		}
		log.Printf("Archive: moved %s (%s) to %s as %q (%d file(s)); removed from Radarr with an exclusion", name, why, a.Config.MovieArchiveDir, stem, len(files))
		if other {
			a.mergeMu.Lock()
			if a.pendingMerges == nil {
				a.pendingMerges = map[int]time.Time{}
			}
			a.pendingMerges[m.TmdbID] = a.now()
			a.mergeMu.Unlock()
		}
		metrics.Inc("plexclean_actions_total", "action", "movie_archived")
	}
}

// archiveName is the archive file name for f: the movie (files[0]) becomes stem+ext, its subtitles keep their
// suffix after the movie's old name (".en.srt").
func archiveName(files []string, f, stem string) string {
	old := strings.TrimSuffix(filepath.Base(files[0]), filepath.Ext(files[0]))
	return stem + strings.TrimPrefix(filepath.Base(f), old)
}

// copyToArchive copies files into the archive root under the archive name. An existing file of a different size is a
// name clash: nothing is copied then, so the owner can sort it out.
func (a *App) copyToArchive(files []string, stem string) error {
	for _, f := range files {
		dst := filepath.Join(a.Config.MovieArchiveDir, archiveName(files, f, stem))
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
		if err := copyFile(f, filepath.Join(a.Config.MovieArchiveDir, archiveName(files, f, stem)), st.Size()); err != nil {
			return err
		}
	}
	return nil
}
