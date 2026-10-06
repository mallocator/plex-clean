package main

import (
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Sweep removes completed torrents whose downloaded files are gone, e.g. after an episode was deleted
// from Plex or Jellyfin. Paths are qBittorrent's own save paths, so plex-clean must mount the downloads
// share at the same path as the qBittorrent container (/downloads).
func (a *App) Sweep() {
	root := filepath.Clean(a.Config.SweepRoot)
	if entries, err := os.ReadDir(root); err != nil || len(entries) == 0 {
		log.Printf("Sweep: %s is missing or empty (not mounted?), skipping", root)
		return
	}
	if err := a.Qbt.Login(); err != nil {
		log.Printf("Sweep: %v", err)
		return
	}
	torrents, err := a.Qbt.Torrents()
	if err != nil {
		log.Printf("Sweep: %v", err)
		return
	}

	var gone []Torrent
	eligible := 0
	for _, t := range torrents {
		if t.AmountLeft > 0 && t.State != "moving" && t.State != "error" && t.State != "missingFiles" {
			continue // still downloading
		}
		if slices.Contains(a.Config.SweepSkipCats, t.Category) {
			continue // Sonarr/Radarr remove their own torrents
		}
		save := filepath.Clean(t.SavePath)
		if save != root && !strings.HasPrefix(save, root+string(os.PathSeparator)) {
			continue // outside the share we can see
		}
		eligible++
		files, err := a.Qbt.Files(t.Hash)
		if err != nil {
			log.Printf("Sweep: files of %s: %v", t.Name, err)
			continue
		}
		for _, f := range files {
			if f.Priority == 0 {
				continue // not selected for download
			}
			if _, err := os.Stat(filepath.Join(save, f.Name)); os.IsNotExist(err) {
				gone = append(gone, t)
				a.debugf("Sweep: %s is missing %s", t.Name, f.Name)
				break
			}
		}
	}

	// Many torrents vanishing at once, and most of them, looks like a mount problem rather than cleanup.
	if len(gone) > a.Config.SweepMaxRemove && len(gone)*2 > eligible {
		log.Printf("Sweep: %d of %d torrents look deleted; refusing (mount problem?)", len(gone), eligible)
		return
	}
	a.debugf("Sweep: %d torrents, %d checked, %d with deleted files", len(torrents), eligible, len(gone))
	for _, t := range gone {
		if err := a.deletable(t.SavePath); err != nil {
			log.Printf("Sweep: refusing to remove %s: %v", t.Name, err)
			continue
		}
		if a.Config.DryRun {
			log.Printf("[dry run] Sweep: would remove torrent %s (files deleted)", t.Name)
			continue
		}
		if err := a.Qbt.Remove(t.Hash); err != nil {
			log.Printf("Sweep: removing %s: %v", t.Name, err)
			continue
		}
		log.Printf("Sweep: removed torrent %s (files deleted)", t.Name)
	}
}
