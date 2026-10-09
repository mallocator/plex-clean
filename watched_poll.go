package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// WatchedPoll is the safety net for missed webhooks: it asks Jellyfin (every user) and Plex (the owner's account)
// which episodes are marked watched, and queues those marked since WATCHED_LOOKBACK that it hasn't seen yet.
// A server's watched mark is the truth, so an episode marked watched by hand counts too, and a viewing on one server
// counts without waiting for WatchState to sync it to the other.
//
// Safeguards: the first run only records what is already marked (baseline, nothing queued), and a poll finding more
// than WATCHED_BURST_MAX new marks (a mass sync or a "mark season watched") queues none of them and logs them, so a
// faulty sync can't trigger mass deletion. Deletion rules are unchanged: Sonarr tag, then the grace period.
func (a *App) WatchedPoll() {
	since := a.now().Add(-a.Config.WatchedLookback)
	var found []polledWatch
	if a.Jellyfin != nil {
		w, err := a.Jellyfin.PlayedEpisodes(since)
		if err != nil {
			log.Printf("Watched poll: %v", err)
		}
		found = append(found, w...)
	}
	if a.Plex != nil {
		w, err := a.Plex.WatchedEpisodes(since)
		if err != nil {
			log.Printf("Watched poll: %v", err)
		}
		found = append(found, w...)
	}
	seen := a.seenWatched()
	baseline := seen.Empty()
	var fresh []polledWatch
	for _, w := range found {
		if !seen.Has(w.key()) {
			fresh = append(fresh, w)
		}
	}
	for _, w := range fresh {
		seen.Add(w.key(), w.At)
	}
	seen.Prune(since.Add(-24 * time.Hour))
	if err := seen.Save(); err != nil {
		log.Printf("Watched poll: saving %s: %v", seen.path, err)
	}
	switch {
	case baseline:
		log.Printf("Watched poll: first run, recorded %d watched marks since %s as known (nothing queued)", len(fresh), since.Format(time.RFC3339))
		return
	case len(fresh) > a.Config.WatchedBurstMax:
		log.Printf("Watched poll: %d new watched marks at once (more than %d), probably a sync; not queuing them:", len(fresh), a.Config.WatchedBurstMax)
		for _, w := range fresh {
			log.Printf("  held: %s", w)
		}
		metrics.Add("plexclean_watched_poll_held_total", float64(len(fresh)))
		return
	}
	for _, w := range fresh {
		a.watchedAt(WatchEvent{Source: w.Source + "-poll", Type: "episode", Series: w.Series, Season: w.Season,
			Episode: w.Episode, Title: w.Title}, w.At)
	}
}

// polledWatch is an episode a server reports as watched.
type polledWatch struct {
	Source  string // "jellyfin" or "plex"
	User    string
	ItemID  string
	Series  string
	Season  int
	Episode int
	Title   string
	At      time.Time // when it was played or marked watched
}

// key identifies one watched mark; watching the episode again later is a new mark.
func (w polledWatch) key() string {
	return fmt.Sprintf("%s|%s|%s|%d", w.Source, w.User, w.ItemID, w.At.Unix())
}

func (w polledWatch) String() string {
	return fmt.Sprintf("%s S%02dE%02d (%s %s, %s)", w.Series, w.Season, w.Episode, w.Source, w.User, w.At.Format("2006-01-02 15:04"))
}

// PlayedEpisodes returns the episodes each Jellyfin user marked played since the given time.
func (j *Jellyfin) PlayedEpisodes(since time.Time) ([]polledWatch, error) {
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := j.get("/Users", &users); err != nil {
		return nil, err
	}
	var out []polledWatch
	for _, u := range users {
		for start := 0; ; start += 100 {
			var page struct {
				Items []struct {
					ID         string `json:"Id"`
					Name       string `json:"Name"`
					SeriesName string `json:"SeriesName"`
					Season     int    `json:"ParentIndexNumber"`
					Episode    int    `json:"IndexNumber"`
					UserData   struct {
						LastPlayedDate string `json:"LastPlayedDate"`
					} `json:"UserData"`
				} `json:"Items"`
			}
			q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Episode"}, "IsPlayed": {"true"},
				"SortBy": {"DatePlayed"}, "SortOrder": {"Descending"}, "EnableUserData": {"true"},
				"StartIndex": {strconv.Itoa(start)}, "Limit": {"100"}}
			if err := j.get("/Users/"+u.ID+"/Items?"+q.Encode(), &page); err != nil {
				return out, err
			}
			older := false
			for _, it := range page.Items {
				at, err := time.Parse(time.RFC3339Nano, it.UserData.LastPlayedDate)
				if err != nil || it.SeriesName == "" {
					continue // marked played without a date: can't tell when, so it can't be "new"
				}
				if at.Before(since) {
					older = true
					break
				}
				out = append(out, polledWatch{Source: "jellyfin", User: u.Name, ItemID: it.ID, Series: it.SeriesName,
					Season: it.Season, Episode: it.Episode, Title: it.Name, At: at})
			}
			if older || len(page.Items) < 100 {
				break
			}
		}
	}
	return out, nil
}

// WatchedEpisodes returns the episodes the Plex owner account (the tokenless local API) watched since the given time.
// Other Plex accounts are covered by the media.scrobble webhook.
func (p *Plex) WatchedEpisodes(since time.Time) ([]polledWatch, error) {
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.get("/library/sections", &sections); err != nil {
		return nil, err
	}
	var out []polledWatch
	for _, d := range sections.MediaContainer.Directory {
		if d.Type != "show" {
			continue
		}
		var items struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey  string `json:"ratingKey"`
					Title      string `json:"title"`
					Series     string `json:"grandparentTitle"`
					Season     int    `json:"parentIndex"`
					Episode    int    `json:"index"`
					LastViewed int64  `json:"lastViewedAt"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		path := fmt.Sprintf("/library/sections/%s/all?type=4&sort=lastViewedAt:desc&lastViewedAt%%3E%%3E=%d", d.Key, since.Unix())
		if err := p.get(path, &items); err != nil {
			return out, err
		}
		for _, m := range items.MediaContainer.Metadata {
			if m.LastViewed == 0 || m.Series == "" {
				continue
			}
			out = append(out, polledWatch{Source: "plex", User: "owner", ItemID: m.RatingKey, Series: m.Series,
				Season: m.Season, Episode: m.Episode, Title: m.Title, At: time.Unix(m.LastViewed, 0)})
		}
	}
	return out, nil
}

// seenSet remembers the watched marks already handled (key -> unix time of the mark), persisted next to the queue.
type seenSet struct {
	mu     sync.Mutex
	path   string
	marks  map[string]int64
	loaded bool // the file existed: not the first run
}

func (a *App) seenWatched() *seenSet {
	a.seenOnce.Do(func() {
		a.seen = &seenSet{path: a.Config.WatchedSeenFile, marks: map[string]int64{}}
		data, err := os.ReadFile(a.seen.path)
		if err == nil && json.Unmarshal(data, &a.seen.marks) == nil {
			a.seen.loaded = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("Watched poll: reading %s: %v", a.seen.path, err)
		}
	})
	return a.seen
}

func (s *seenSet) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.loaded
}

func (s *seenSet) Has(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.marks[k]
	return ok
}

func (s *seenSet) Add(k string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks[k] = at.Unix()
}

func (s *seenSet) Prune(before time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.marks {
		if t < before.Unix() {
			delete(s.marks, k)
		}
	}
}

func (s *seenSet) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.marks)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.loaded = true
	return nil
}
