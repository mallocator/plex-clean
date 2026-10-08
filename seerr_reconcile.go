package main

import (
	"fmt"
	"log"
)

// SeerrReconcile resets Seerr TV records that are stuck in "processing" after their series left Sonarr.
//
// Seerr's own Sonarr scan only cleans up records with a stored TVDB ID, and it never backfills that ID on a record
// created from TMDB alone. Such a record keeps its seasons "processing" forever, which also blocks requesting new
// seasons. A record is reset (DELETE /api/v1/media/<id>: Seerr forgets the status, files are untouched) only when
//   - the series is in Sonarr neither by TVDB nor by TMDB ID,
//   - no Seerr request references it, and
//   - no season is (partially) available.
//
// Runs every SEERR_RECONCILE_INTERVAL (default 24h). Refuses to act if Sonarr reports no series at all (API trouble)
// and resets at most SeerrResetMax records per run.
func (a *App) SeerrReconcile() {
	if a.Seerr == nil || a.Sonarr == nil {
		return
	}
	var series []struct {
		TVDB int `json:"tvdbId"`
		TMDB int `json:"tmdbId"`
	}
	if err := a.Sonarr.arr().do("GET", "/api/v3/series", nil, &series); err != nil {
		log.Printf("Seerr reconcile: %v", err)
		return
	}
	if len(series) == 0 {
		log.Printf("Seerr reconcile: Sonarr reports no series, skipping (API problem?)")
		return
	}
	tvdb, tmdb := map[int]bool{}, map[int]bool{}
	for _, s := range series {
		if s.TVDB > 0 {
			tvdb[s.TVDB] = true
		}
		if s.TMDB > 0 {
			tmdb[s.TMDB] = true
		}
	}

	requested, err := a.Seerr.requestedMedia()
	if err != nil {
		log.Printf("Seerr reconcile: %v", err)
		return
	}
	stuck, err := a.Seerr.processingTV()
	if err != nil {
		log.Printf("Seerr reconcile: %v", err)
		return
	}

	reset := 0
	for _, m := range stuck {
		if (m.TVDB > 0 && tvdb[m.TVDB]) || tmdb[m.TMDB] || requested[m.ID] || m.anyAvailable() {
			continue
		}
		if reset >= SeerrResetMax {
			log.Printf("Seerr reconcile: limit of %d resets reached, the rest waits for the next run", SeerrResetMax)
			return
		}
		if a.Config.DryRun {
			log.Printf("[dry run] Seerr reconcile: would reset TMDB %d (record %d): processing, not in Sonarr, no request", m.TMDB, m.ID)
			continue
		}
		if _, err := a.Seerr.do("DELETE", fmt.Sprintf("/api/v1/media/%d", m.ID), nil, nil); err != nil {
			log.Printf("Seerr reconcile: resetting TMDB %d: %v", m.TMDB, err)
			continue
		}
		reset++
		metrics.Inc("plexclean_actions_total", "action", "seerr_reset")
		log.Printf("Seerr reconcile: reset TMDB %d (record %d): it was processing although the series is no longer in Sonarr and nobody requested it", m.TMDB, m.ID)
	}
}

// SeerrResetMax caps the resets per run.
const SeerrResetMax = 50

type seerrMedia struct {
	ID        int    `json:"id"`
	MediaType string `json:"mediaType"`
	TMDB      int    `json:"tmdbId"`
	TVDB      int    `json:"tvdbId"`
	Status    int    `json:"status"`
	Seasons   []struct {
		Number int `json:"seasonNumber"`
		Status int `json:"status"`
	} `json:"seasons"`
}

// Seerr media status values: 3 processing, 4 partially available, 5 available.
func (m seerrMedia) anyAvailable() bool {
	if m.Status == 4 || m.Status == 5 {
		return true
	}
	for _, s := range m.Seasons {
		if s.Status == 4 || s.Status == 5 {
			return true
		}
	}
	return false
}

// processingTV returns the TV records Seerr lists as processing.
func (s *Seerr) processingTV() ([]seerrMedia, error) {
	var out []seerrMedia
	for skip := 0; ; skip += 100 {
		var page struct {
			PageInfo struct {
				Pages int `json:"pages"`
				Page  int `json:"page"`
			} `json:"pageInfo"`
			Results []seerrMedia `json:"results"`
		}
		if _, err := s.do("GET", fmt.Sprintf("/api/v1/media?take=100&skip=%d&filter=processing&sort=added", skip), nil, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Results {
			if m.MediaType == "tv" {
				out = append(out, m)
			}
		}
		if len(page.Results) < 100 {
			return out, nil
		}
	}
}

// requestedMedia returns the media record IDs any request refers to.
func (s *Seerr) requestedMedia() (map[int]bool, error) {
	ids := map[int]bool{}
	for skip := 0; ; skip += 100 {
		var page struct {
			Results []struct {
				Media *struct {
					ID int `json:"id"`
				} `json:"media"`
			} `json:"results"`
		}
		if _, err := s.do("GET", fmt.Sprintf("/api/v1/request?take=100&skip=%d&filter=all", skip), nil, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Results {
			if r.Media != nil {
				ids[r.Media.ID] = true
			}
		}
		if len(page.Results) < 100 {
			return ids, nil
		}
	}
}
