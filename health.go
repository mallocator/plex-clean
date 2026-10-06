package main

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

// Download health: Sonarr and Radarr leave broken downloads in their queue forever. Every sweep, plex-clean
// removes them from qBittorrent through the app's queue, blocklists the release and lets the app search again:
//   - fakes and unusable downloads: an executable or an archive instead of a video, or nothing importable;
//   - stalled downloads: no data (or no metadata) for STALL_TIMEOUT.

type queueItem struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	DownloadID     string `json:"downloadId"`
	Protocol       string `json:"protocol"`
	TrackedState   string `json:"trackedDownloadState"`
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
			reason := q.unusable()
			if t, ok := byHash[strings.ToLower(q.DownloadID)]; ok && reason == "" && a.Config.StallTimeout > 0 &&
				stalled(t, a.now(), a.Config.StallTimeout) {
				reason = fmt.Sprintf("stalled (%s) for over %s", t.State, a.Config.StallTimeout)
			}
			if reason == "" {
				continue
			}
			if removed >= a.Config.SweepMaxRemove {
				log.Printf("Downloads: limit of %d removals reached, the rest waits for the next run", a.Config.SweepMaxRemove)
				return
			}
			if a.Config.DryRun {
				log.Printf("[dry run] Downloads (%s): would reject %s: %s", name, q.Title, reason)
				continue
			}
			if err := app.Reject(q.ID); err != nil {
				log.Printf("Downloads (%s): rejecting %s: %v", name, q.Title, err)
				continue
			}
			removed++
			log.Printf("Downloads (%s): rejected %s (%s); blocklisted, searching again", name, q.Title, reason)
		}
	}
}
