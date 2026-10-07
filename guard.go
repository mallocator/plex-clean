package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The archive (Video/Series, Video/Anime, Video/Movies) belongs to the owner: nothing automatic deletes or moves files
// there. Every deletion plex-clean makes, directly or through Sonarr/Radarr/qBittorrent, first passes deletable: the
// path must be below one of DELETE_ROOTS (the downloads share) and outside the archive directories and PROTECTED_DIRS.
func (a *App) deletable(path string) error {
	p := filepath.Clean(path)
	for _, d := range append([]string{a.Config.ArchiveDir, a.Config.AnimeArchiveDir, a.Config.MovieArchiveDir}, a.Config.ProtectedDirs...) {
		if d != "" && within(p, d) {
			return fmt.Errorf("%s is in the archive (%s), which only the owner changes", p, d)
		}
	}
	if len(a.Config.DeleteRoots) == 0 {
		return nil
	}
	for _, r := range a.Config.DeleteRoots {
		if within(p, r) {
			return nil
		}
	}
	return fmt.Errorf("%s is outside %s, the only places plex-clean deletes in", p, strings.Join(a.Config.DeleteRoots, ", "))
}

func within(p, dir string) bool {
	dir = filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator))
}
