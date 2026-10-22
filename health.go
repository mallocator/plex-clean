package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Download health: Sonarr and Radarr leave broken downloads in their queue forever. Every sweep, curator
// removes them from qBittorrent through the app's queue, blocklists the release and lets the app search again:
//   - fakes and unusable downloads: an executable or an archive instead of a video, or nothing importable;
//   - stalled downloads: no data (or no metadata) for STALL_TIMEOUT.
// A download whose video is already gone from the downloads share was moved by an import that failed halfway (the
// file reached the series folder, the database write didn't, e.g. "database is locked" on a busy disk). Sonarr then
// reports "no files found are eligible" too, but the release is good: rescan the series/movie so the app finds the
// file, and drop the queue entry without blocklisting or searching again.

type queueItem struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	DownloadID     string `json:"downloadId"`
	Protocol       string `json:"protocol"`
	TrackedState   string `json:"trackedDownloadState"`
	OutputPath     string `json:"outputPath"`
	SeriesID       int    `json:"seriesId"`  // Sonarr
	EpisodeID      int    `json:"episodeId"` // Sonarr
	MovieID        int    `json:"movieId"`   // Radarr
	StatusMessages []struct {
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
}

// unusableReasons are Sonarr/Radarr import warnings that mean the download will never import.
var unusableReasons = []string{"executable file", "archive file", "no files found are eligible", "sample"}

func (q queueItem) unusable() string {
	if q.TrackedState != "importPending" && q.TrackedState != "importBlocked" && q.TrackedState != "importFailed" {
		return ""
	}
	for _, sm := range q.StatusMessages {
		for _, m := range sm.Messages {
			for _, r := range unusableReasons {
				if strings.Contains(strings.ToLower(m), r) {
					return m
				}
			}
		}
	}
	return ""
}

// junkExts are the files of fakes: a folder holding one of these isn't an emptied download.
var junkExts = []string{".exe", ".lnk", ".scr", ".bat", ".cmd", ".msi", ".zip", ".rar", ".7z", ".iso"}

// alreadyMoved reports a download whose content has been moved away: the path is gone, or it's a folder left with
// extras only (no video, nothing a fake would hold). The parent must exist, so an unmounted share never qualifies.
func alreadyMoved(path string) bool {
	if path == "" {
		return false
	}
	p := filepath.Clean(path)
	if st, err := os.Stat(filepath.Dir(p)); err != nil || !st.IsDir() {
		return false
	}
	st, err := os.Stat(p)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || !st.IsDir() {
		return false
	}
	moved := true
	filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			moved = false
			return filepath.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(f))
		if !d.IsDir() && (slices.Contains(videoExts, ext) || slices.Contains(junkExts, ext)) {
			moved = false
			return filepath.SkipAll
		}
		return nil
	})
	return moved
}

// downloadingStates are the states of a torrent that should be receiving data (queued ones wait for a slot).
var downloadingStates = []string{"downloading", "forcedDL", "stalledDL", "metaDL", "forcedMetaDL"}

// stalled reports an incomplete torrent that hasn't received anything for longer than timeout. Progress, not
// amount_left, decides "incomplete": without metadata the size (and so amount_left) is still 0.
func stalled(t Torrent, now time.Time, timeout time.Duration) bool {
	if t.Progress >= 1 || !slices.Contains(downloadingStates, t.State) {
		return false
	}
	last := max(t.AddedOn, t.LastActivity)
	return last > 0 && now.Sub(time.Unix(last, 0)) > timeout
}

func (a *Arr) Queue() ([]queueItem, error) {
	var page struct {
		Records []queueItem `json:"records"`
	}
	err := a.do("GET", "/api/v3/queue?pageSize=500", nil, &page)
	return page.Records, err
}

// Reject removes a download from the client, blocklists its release and lets the app search for another.
func (a *Arr) Reject(id int) error {
	return a.do("DELETE", fmt.Sprintf("/api/v3/queue/%d?removeFromClient=true&blocklist=true&skipRedownload=false", id), nil, nil)
}

// Remove drops a download from the queue and the client without blocklisting it or searching again.
func (a *Arr) Remove(id int) error {
	return a.do("DELETE", fmt.Sprintf("/api/v3/queue/%d?removeFromClient=true&blocklist=false&skipRedownload=true", id), nil, nil)
}

// Rescan has the app scan the item's series (Sonarr) or movie (Radarr) folder for files it doesn't know yet.
func (a *Arr) Rescan(q queueItem) error {
	switch {
	case q.SeriesID > 0:
		return a.do("POST", "/api/v3/command", map[string]any{"name": "RescanSeries", "seriesId": q.SeriesID}, nil)
	case q.MovieID > 0:
		return a.do("POST", "/api/v3/command", map[string]any{"name": "RescanMovie", "movieId": q.MovieID}, nil)
	}
	return fmt.Errorf("no series or movie ID")
}

// CheckDownloads applies the download health rules to Sonarr's and Radarr's queues.
func (a *App) CheckDownloads() {
	if a.Qbt == nil || (a.Sonarr == nil && a.Radarr == nil) {
		return
	}
	if err := a.Qbt.Login(); err != nil {
		log.Printf("Downloads: %v", err)
		return
	}
	torrents, err := a.Qbt.Torrents()
	if err != nil {
		log.Printf("Downloads: %v", err)
		return
	}
	byHash := map[string]Torrent{}
	for _, t := range torrents {
		byHash[strings.ToLower(t.Hash)] = t
	}
	apps := map[string]*Arr{}
	if a.Sonarr != nil {
		apps["Sonarr"] = a.Sonarr.arr()
	}
	if a.Radarr != nil {
		apps["Radarr"] = a.Radarr
	}
	removed := 0
	for name, app := range apps {
		items, err := app.Queue()
		if err != nil {
			log.Printf("Downloads (%s): %v", name, err)
			continue
		}
		for _, q := range items {
			if q.Protocol != "" && q.Protocol != "torrent" {
				continue
			}
			t, known := byHash[strings.ToLower(q.DownloadID)]
			reason := q.unusable()
			if known && reason == "" && a.Config.StallTimeout > 0 && stalled(t, a.now(), a.Config.StallTimeout) {
				reason = fmt.Sprintf("stalled (%s) for over %s", t.State, a.Config.StallTimeout)
			}
			if reason == "" {
				continue
			}
			if !known {
				continue // rejecting deletes the torrent's data: only for torrents whose location we can check
			}
			if err := a.deletable(t.SavePath); err != nil {
				log.Printf("Downloads (%s): refusing to reject %s: %v", name, q.Title, err)
				continue
			}
			if removed >= a.Config.SweepMaxRemove {
				log.Printf("Downloads: limit of %d removals reached, the rest waits for the next run", a.Config.SweepMaxRemove)
				return
			}
			moved := strings.Contains(strings.ToLower(reason), "no files found are eligible") && alreadyMoved(q.OutputPath)
			if a.Config.DryRun {
				if moved {
					log.Printf("[dry run] Downloads (%s): would rescan and remove %s (files already moved out of %s)", name, q.Title, q.OutputPath)
				} else {
					log.Printf("[dry run] Downloads (%s): would reject %s: %s", name, q.Title, reason)
				}
				continue
			}
			if moved {
				if err := app.Rescan(q); err != nil {
					log.Printf("Downloads (%s): rescanning for %s: %v", name, q.Title, err)
					continue
				}
				if err := app.Remove(q.ID); err != nil {
					log.Printf("Downloads (%s): removing %s: %v", name, q.Title, err)
					continue
				}
				removed++
				log.Printf("Downloads (%s): %s was already moved out of %s (import failed halfway); rescanned, removed without blocklisting", name, q.Title, q.OutputPath)
				metrics.Inc("curator_actions_total", "action", "download_rescanned", "reason", "moved")
				continue
			}
			if err := app.Reject(q.ID); err != nil {
				log.Printf("Downloads (%s): rejecting %s: %v", name, q.Title, err)
				continue
			}
			removed++
			log.Printf("Downloads (%s): rejected %s (%s); blocklisted, searching again", name, q.Title, reason)
			kind := "unusable"
			if strings.HasPrefix(reason, "stalled") {
				kind = "stalled"
			}
			metrics.Inc("curator_actions_total", "action", "download_rejected", "reason", kind)
		}
	}
}
